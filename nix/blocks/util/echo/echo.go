// Command echo-server is the runtime for the util/echo block: a dependency-free
// static HTTP responder that echoes a configurable body on a configurable port.
// It starts in well under a second (pure stdlib, no warm-up) and doubles as the
// scheduler/lifecycle test workload in every later VM and chaos test.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("ECHO_PORT")
	if port == "" {
		port = "18080"
	}
	body := os.Getenv("ECHO_BODY")

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%s", port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "echo-server: %v\n", err)
		os.Exit(1)
	}
}
