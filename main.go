package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.ngrok.com/ngrok"
	"golang.ngrok.com/ngrok/config"

	"golang.org/x/time/rate"

	_ "github.com/lib/pq"
)

var (
	Listen = envOr(
		"LISTEN",
		fmt.Sprintf(":%s", envOr("PORT", "10000")),
	)
	OtherPorts = func() []string {
		ports := os.Getenv("PORTS")
		ports = strings.TrimSpace(ports)
		if len(ports) == 0 {
			return nil
		}
		return strings.Split(ports, ",")
	}()
	RPS  = mustFloat(envOr("RPS", "3"))
	JSON = os.Getenv("JSON") != ""

	PSQLConnString = os.Getenv("PSQL_CONN_STRING")

	NGrokToken = os.Getenv("NGROK_TOKEN")
)

func envOr(k, v string) string {
	if v2 := os.Getenv(k); v2 != "" {
		return v2
	}
	return v
}

func mustFloat(s string) float64 {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v
	}
	if v, err := strconv.ParseInt(s, 10, 32); err == nil {
		return float64(v)
	}
	panic(fmt.Errorf("%s is not a float", s))
}

func main() {
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	if err := fs.Parse(os.Args[1:]); err != nil {
		panic(err)
	}
	ctx, can := signal.NotifyContext(context.Background(), syscall.SIGINT)
	defer can()
	if err := run(ctx); err != nil && ctx.Err() == nil {
		panic(err)
	}
}

func run(ctx context.Context) error {
	for _, otherPort := range OtherPorts {
		go _httpListen(ctx, ":"+otherPort, http.HandlerFunc(http.NotFound))
	}

	accessLogDB := MaybeDial(ctx)
	defer accessLogDB.Close()

	if NGrokToken == "" {
	} else if err := AcquireDistributedLock(ctx, accessLogDB); err != nil {
		return fmt.Errorf("failed to acquire distributed lock: %w", err)
	}

	limiter := rate.NewLimiter(rate.Limit(RPS), 1)
	lastNRequests := make([]any, 0, 50)
	s := &http.Server{
		Addr: Listen,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, can := context.WithTimeout(r.Context(), time.Minute)
			defer can()

			if r.URL.Path == "/__history__" {
				json.NewEncoder(w).Encode(lastNRequests)
				return
			}

			limiter.Wait(r.Context())
			headers, _ := json.MarshalIndent(r.Header, "	", "	")

			var reader io.Reader = r.Body
			switch r.Header.Get("Content-Encoding") {
			case "zstd":
				r, err := zstd.NewReader(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				defer r.Close()
				reader = r
			}

			body, err := io.ReadAll(reader)
			if err != nil {
				body = []byte(fmt.Sprintf("(failed to read body: %v)", err))
			}

			var outputPayload any = fmt.Sprintf("[%s] %s %s\n%s\n	(%d==%d) %s\n",
				time.Now(), r.Method, r.URL,
				headers,
				len(body), r.ContentLength,
				body,
			)
			output := fmt.Sprint(outputPayload)
			if JSON {
				outputPayload = map[string]any{
					"now":            time.Now(),
					"method":         r.Method,
					"url":            r.URL.String(),
					"headers":        r.Header,
					"body-length":    len(body),
					"content-length": r.ContentLength,
					"body":           string(body),
				}
				b, _ := json.Marshal(outputPayload)
				output = string(b)
			}

			lastNRequests = append(lastNRequests, outputPayload)
			for len(lastNRequests) > 50 {
				lastNRequests = lastNRequests[1:]
			}

			header, _ := json.Marshal(r.Header)
			if _, err := accessLogDB.Exec(ctx, `
				CREATE TABLE IF NOT EXISTS http_access_log (
					at TIMESTAMP
					, method TEXT
					, url TEXT
					, headers TEXT
					, body TEXT
				)
			`); err != nil {
				log.Printf("psql: error ensuring table: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			} else if _, err := accessLogDB.Exec(ctx, `
				INSERT INTO http_access_log (at, method, url, headers, body)
				VALUES (now(), $1, $2, $3, $4)
			`, r.Method, r.URL.String(), header, base64.StdEncoding.EncodeToString(body)); err != nil {
				log.Printf("psql: error inserting into table: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			for _, w := range []io.Writer{w, log.Writer()} {
				fmt.Fprintf(w, "%s\n", output)
			}
		}),
	}
	defer s.Close()

	ngrokURL, err := ngrokListen(ctx, s)
	if err != nil {
		return fmt.Errorf("failed to ngrok listen: %w", err)
	}

	go httpListen(ctx, s)

	logEnv(ngrokURL)

	<-ctx.Done()
	return ctx.Err()
}

type DB interface {
	Close()
	Exec(context.Context, string, ...any) (int64, error)
}

func MaybeDial(ctx context.Context) DB {
	if PSQLConnString == "" {
		return nodb{}
	}

	defer log.Printf("/dialed psql")

	if u, err := url.Parse(PSQLConnString); err != nil || u.Scheme == "" {
	} else if err := blockUntilTCP(ctx, u.Host); err != nil {
		panic(err)
	} else if err := func() error {
		u2 := *u
		u2.Path = "/postgres"

		db, err := blockUntilPSQL(ctx, u2.String())
		if err != nil {
			return err
		}
		defer db.Close()

		dbname := path.Base(u.Path)

		var n int
		err = db.DB.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM pg_database WHERE datname=$1
		`, dbname).Scan(&n)
		if err != nil || n < 1 {
			if _, err := db.Exec(ctx, `CREATE DATABASE `+dbname); err != nil {
				return fmt.Errorf("no dbname %q but failed to create: %w", dbname, err)
			}
		}

		return nil
	}(); err != nil {
		panic(err)
	}

	db, err := blockUntilPSQL(ctx, PSQLConnString)
	if err != nil {
		panic(err)
	}
	return db
}

func blockUntilTCP(ctx context.Context, addr string) error {
	if !strings.Contains(addr, ":") {
		addr += ":5432"
	}

	log.Printf("tcp dialing %s...", addr)
	defer log.Printf("/tcp dialed %s", addr)

	d := net.Dialer{}
	return retry(ctx, func(ctx context.Context) error {
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		defer c.Close()

		return nil
	})
}

func blockUntilPSQL(ctx context.Context, connURL string) (db, error) {
	defer log.Printf("/psqld %s", connURL)

	var some *sql.DB
	err := retry(ctx, func(ctx context.Context) error {
		log.Printf("psqling %s...", connURL)

		a, err := sql.Open("postgres", connURL)
		if err != nil {
			return err
		}
		if err := a.PingContext(ctx); err != nil {
			defer a.Close()
			return err
		}
		some = a
		return nil
	})
	return db{DB: some}, err
}

func retry(ctx context.Context, foo func(context.Context) error) error {
	var lastErr error
	for ctx.Err() == nil {
		subctx, can := context.WithTimeout(ctx, 5*time.Second)
		defer can()

		lastErr = foo(subctx)
		if lastErr == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return ctx.Err()
}

type nodb struct{}

func (nodb) Close() {}

func (nodb) Exec(context.Context, string, ...any) (int64, error) {
	return 1, nil
}

type db struct {
	*sql.DB
}

func (db db) Close() { db.DB.Close() }

func (db db) Exec(ctx context.Context, q string, args ...any) (int64, error) {
	result, err := db.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func AcquireDistributedLock(ctx context.Context, db DB) error {
	log.Printf("acquiring lock...")
	defer log.Printf("/acquired lock")

	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS distributed_lock(pk TEXT PRIMARY KEY, holder TEXT, last_seen_at TIMESTAMP)`); err != nil {
		return fmt.Errorf("failed to init distributed locking: %w", err)
	}

	acquiredCh := make(chan struct{}, 0)
	acquired := sync.OnceFunc(func() {
		close(acquiredCh)
	})
	go func() {
		me := time.Now().String()
		c := time.NewTicker(5 * time.Second)
		defer c.Stop()
		for range c.C {
			n, err := db.Exec(ctx, `
				INSERT INTO distributed_lock
					(pk, holder, last_seen_at)
				VALUES
					('only', $1, now())
				ON CONFLICT (pk) DO UPDATE
					SET holder=$1, last_seen_at=now()
					WHERE distributed_lock.holder=$1 OR now()-distributed_lock.last_seen_at>interval '10 seconds'
			`, me)
			if err != nil {
				continue
			}
			if n > 0 {
				acquired()
			}
		}
	}()

	select {
	case <-acquiredCh:
	case <-ctx.Done():
	}
	return ctx.Err()
}

func ngrokListen(ctx context.Context, s *http.Server) (string, error) {
	if NGrokToken == "" {
		return "", nil
	}

	log.Printf("ngrokking...")
	listener, err := ngrok.Listen(ctx, config.HTTPEndpoint(), ngrok.WithAuthtoken(NGrokToken))
	if err != nil {
		return "", err
	}
	defer listener.Close()

	go func() {
		if err := http.Serve(listener, s.Handler); err != nil && ctx.Err() == nil {
			panic(err)
		}
	}()

	return listener.URL(), nil
}

func httpListen(ctx context.Context, s *http.Server) {
	log.Printf("listening on %s", s.Addr)
	_httpListen(ctx, Listen, s.Handler)
}

func _httpListen(ctx context.Context, listen string, h http.Handler) {
	if err := http.ListenAndServe(listen, h); err != nil && ctx.Err() == nil {
		panic(err)
	}
}

func logEnv(ngrokURL string) {
	m := map[string]any{
		"$RENDER_EXTERNAL_URL": os.Getenv("RENDER_EXTERNAL_URL"),
		"Listen":               Listen,
		"ngrokURL":             ngrokURL,
	}
	b, _ := json.Marshal(m)
	log.Printf("env | %s", b)
}
