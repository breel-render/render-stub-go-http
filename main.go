package main

import (
	"net/http"
	"time"
)

func main() {
	next := time.Now()
	if err := http.ListenAndServe(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if toWait := next.Sub(time.Now()); toWait > 0 {
			time.Sleep(toWait)
		}
		next = time.Now().Add(time.Second)
		w.Write([]byte("hello world"))
	})); err != nil {
		panic(err)
	}
}
