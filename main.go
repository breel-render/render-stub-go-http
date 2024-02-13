package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/klauspost/compress/zstd"

	"golang.org/x/time/rate"
)

var (
	Listen = envOr(
		"LISTEN",
		fmt.Sprintf(":%s", envOr("PORT", "10000")),
	)
	RPS  = mustFloat(envOr("RPS", "3"))
	JSON = os.Getenv("JSON") != ""
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
	limiter := rate.NewLimiter(rate.Limit(RPS), 1)
	if err := http.ListenAndServe(Listen, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limiter.Wait(r.Context())
		headers, _ := json.MarshalIndent(r.Header, "   ", "   ")

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

		output := fmt.Sprintf("[%s] %s %s\n%s\n   (%d==%d) %s\n",
			time.Now(), r.Method, r.URL,
			headers,
			len(body), r.ContentLength,
			body,
		)
		if JSON {
			js, _ := json.Marshal(map[string]any{
				"now":            time.Now(),
				"method":         r.Method,
				"url":            r.URL.String(),
				"headers":        r.Header,
				"body-length":    len(body),
				"content-length": r.ContentLength,
				"body":           string(body),
			})
			output = string(js)
		}

		for _, w := range []io.Writer{w, log.Writer()} {
			fmt.Fprintf(w, "%s\n", output)
		}
	})); err != nil {
		panic(err)
	}
}
