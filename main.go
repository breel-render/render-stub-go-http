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
	"os"
	"os/signal"
	"strconv"
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
	RPS  = mustFloat(envOr("RPS", "3"))
	JSON = os.Getenv("JSON") != ""

	PSQLConnString = os.Getenv("PSQL_CONN_STRING")
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
	var accessLogDB *sql.DB
	if PSQLConnString != "" {
		log.Printf("dialing psql...")
		db, err := sql.Open("postgres", PSQLConnString)
		if err != nil {
			return err
		}
		if err := db.PingContext(ctx); err != nil {
			return err
		}
		defer db.Close()
		accessLogDB = db
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

			if accessLogDB != nil {
				header, _ := json.Marshal(r.Header)
				if _, err := accessLogDB.ExecContext(ctx, `
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
				} else if _, err := accessLogDB.ExecContext(ctx, `
					INSERT INTO http_access_log (at, method, url, headers, body)
					VALUES (now(), $1, $2, $3, $4)
				`, r.Method, r.URL.String(), header, base64.URLEncoding.EncodeToString(body)); err != nil {
					log.Printf("psql: error inserting into table: %v", err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}

			for _, w := range []io.Writer{w, log.Writer()} {
				fmt.Fprintf(w, "%s\n", output)
			}
		}),
	}
	defer s.Close()

	ngrokURL := ""
	if token := os.Getenv("NGROK_TOKEN"); token != "" {
		listener, err := ngrok.Listen(ctx, config.HTTPEndpoint(), ngrok.WithAuthtoken(token))
		if err != nil {
			return err
		}
		defer listener.Close()

		go func() {
			if err := http.Serve(listener, s.Handler); err != nil && ctx.Err() == nil {
				panic(err)
			}
		}()

		ngrokURL = listener.URL()
	}

	go func() {
		if err := http.ListenAndServe(Listen, s.Handler); err != nil && ctx.Err() == nil {
			panic(err)
		}
	}()

	{
		m := map[string]any{
			"$RENDER_EXTERNAL_URL": os.Getenv("RENDER_EXTERNAL_URL"),
			"Listen":               Listen,
			"ngrokURL":             ngrokURL,
		}
		b, _ := json.Marshal(m)
		log.Printf("env | %s", b)
	}

	<-ctx.Done()
	return ctx.Err()
}
