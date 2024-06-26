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

	"golang.org/x/time/rate"
)

var (
	Listen = envOr(
		"LISTEN",
		fmt.Sprintf(":%s", envOr("PORT", "8080")),
	)
	RPS = mustFloat(envOr("RPS", "3"))
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
		body, err := io.ReadAll(r.Body)
		if err != nil {
			body = []byte(fmt.Sprintf("(failed to read body: %v)", err))
		}
		for _, w := range []io.Writer{w, log.Writer()} {
			fmt.Fprintf(w, "[%s] %s %s\n%s\n   (%d==%d) %s\n",
				time.Now(), r.Method, r.URL,
				headers,
				len(body), r.ContentLength,
				body,
			)
		}
	})); err != nil {
		panic(err)
	}
}
