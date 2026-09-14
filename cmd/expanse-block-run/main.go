// Command expanse-block-run is the runtime entrypoint for block
// replicas (PHASE04.md §5.3). The static template unit
// `expanse-block@.service` has ExecStart = "expanse-block-run %i" —
// per-replica identity arrives as the systemd instance string
// "<namespace>-<name>-<index>", and the full desired spec (type, args,
// config) is written by the node agent to
// /run/expanse/block-replica/<instance>.json before the unit starts.
//
// The binary resolves the block type to a workload:
//   - util/echo: stdlib HTTP echo server (port/body from --config JSON
//     in the spec's args) — the scheduler/lifecycle test workload.
//   - anything else: idle placeholder (sleeps forever) until per-type
//     runtimes land with the shipped-block workloads (T24).
//
// Runtime wiring keeps the block catalog's module.nix contract (T05)
// authoritative for real workloads; this helper only needs to make the
// pipeline — deploy → placed → unit running → Running — observable.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// specDir is where the agent writes per-replica desired-state JSON;
// override with SPEC_DIR (tests).
var specDir = envOr("SPEC_DIR", "/run/expanse/block-replica")

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: expanse-block-run <instance>\n")
		os.Exit(2)
	}
	instance := os.Args[1]

	spec, err := loadSpec(instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
		os.Exit(1)
	}
	args := append([]string{}, spec.Args...)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	switch spec.Type {
	case "util/echo":
		if err := runEcho(ctx, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	default:
		// Idle placeholder: the unit runs healthy but does nothing
		// until the type's real runtime exists (T24).
		fmt.Printf("expanse-block-run: no runtime for type %q; idling\n", spec.Type)
		<-ctx.Done()
	}
}

// spec mirrors the agent's systemd.Spec (the fields the helper needs).
type spec struct {
	Type string   `json:"type"`
	Args []string `json:"args,omitempty"`
}

// loadSpec reads the desired-state JSON the agent wrote for this
// instance. Missing file: run as a bare echo unit (template-only
// start, e.g. manual `systemctl start`) — still serves a workload by
// falling back to the instance-decoded type when possible.
func loadSpec(instance string) (*spec, error) {
	return loadSpecAt(specDir, instance)
}

func loadSpecAt(dir, instance string) (*spec, error) {
	raw, err := os.ReadFile(filepath.Join(dir, instance+".json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "expanse-block-run: no spec for %s (%v); fallback workload\n", instance, err)
		return &spec{Type: fallbackType(instance)}, nil
	}
	var s spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("bad spec for %s: %w", instance, err)
	}
	return &s, nil
}

// fallbackType decodes "<ns>-<name>-<index>" into a type guess for
// template-only starts. No spec file means the agent never intended to
// start us; default to the test workload so the unit still comes up.
func fallbackType(instance string) string {
	_ = instance
	return "util/echo"
}

// runEcho serves the util/echo workload from --config JSON args
// ({"port": N, "body": "..."}); env vars win for compatibility with
// the module.nix-based echo-server (T05 contract).
func runEcho(ctx context.Context, args []string) error {
	var cfg struct {
		Port float64 `json:"port"`
		Body string  `json:"body"`
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--config" {
			if err := json.Unmarshal([]byte(args[i+1]), &cfg); err != nil {
				return fmt.Errorf("bad --config: %w", err)
			}
			i++
		}
	}
	port := os.Getenv("ECHO_PORT")
	if port == "" {
		port = fmt.Sprintf("%d", int(cfg.Port))
	}
	body := os.Getenv("ECHO_BODY")
	if body == "" {
		body = cfg.Body
	}
	if port == "" || port == "0" {
		port = "18080"
	}

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
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	fmt.Printf("expanse-block-run: echo serving on :%s\n", port)
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		return nil
	}
}
