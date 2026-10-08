// Command expanse-block-run is the runtime entrypoint for block
// replicas (PHASE04.md §5.3). The static template unit
// `expanse-block@.service` has ExecStart = "expanse-block-run %i" —
// per-replica identity arrives as the systemd instance string
// "<namespace>-<name>-<index>", and the full desired spec (type, args,
// config) is written by the node agent to
// /run/expanse/block-replica/<instance>.json before the unit starts.
//
// The binary resolves the block type to a workload (shipped types in
// workloads.go):
//   - util/echo: stdlib HTTP echo server
//   - web/nginx, db/redis, monitor/node-exporter, monitor/prometheus,
//     monitor/grafana, ai/ollama: upstream
//     binaries from PATH (config → flags/generated conf)
//   - web/static-site: native Go file server from the config index
//   - share/smb: upstream smbd, config generated from spec.config plus
//     the bound volume's mountPath (via mountPaths/firstMount)
//   - share/nfs: upstream NFS-Ganesha (nfs.go), one NFSv4 export with its
//     client-recovery state on the bound volume
//   - storage/s3: upstream Garage (garage.go), single-node S3 with every
//     piece of its state on the bound volume
//   - db/mariadb: upstream mariadbd (mariadb.go), its datadir on the bound
//     volume and its accounts converged by an init file on every start
//   - web/caddy: upstream caddy (caddy.go), its certificates on the bound
//     volume and its Caddyfile generated from spec.config.sites
//   - net/haproxy: upstream haproxy (haproxy.go), stateless, haproxy.cfg
//     generated from spec.config.frontends
//   - iscsi/target: LIO, driven via targetcli-fb's one-shot CLI form,
//     against the bound raw volume's DRBD device (via voldevs/
//     firstVoldev + waitForPrimaryDevice, PHASE-04-TASKS.md D3)
//   - db/postgres: upstream postgres, bootstrapped per the lease-gated
//     election controller's role file (internal/blocks/pgha) — initdb a
//     fresh primary, or pg_basebackup from the elected one
//   - vm/instance: upstream qemu-kvm, exec'd directly (no libvirt)
//     against the bound raw volume as its disk and a macvtap child of
//     the node's uplink as its network identity (PHASE-06-TASKS.md D1/D2)
//   - anything else: idle placeholder (unknown catalog types have no
//     runtime contract yet; they come up healthy but idle)
//
// Pipeline (deploy → placed → unit running → Running) observable.
package main

import (
	"context"
	"encoding/json"
	"errors"
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
	case "web/nginx":
		if err := runNginx(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "web/whoami":
		if err := runWhoami(ctx, spec.Index, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "db/redis":
		if err := runRedis(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "monitor/node-exporter":
		if err := runNodeExporter(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "monitor/prometheus":
		if err := runPrometheus(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "monitor/grafana":
		if err := runGrafana(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "ai/ollama":
		if err := runOllama(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "web/static-site":
		if err := runStaticSite(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "share/smb":
		if err := runSMB(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "share/nfs":
		if err := runNFS(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "storage/s3":
		if err := runS3(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "db/mariadb":
		if err := runMariaDB(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "web/caddy":
		if err := runCaddy(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "net/haproxy":
		if err := runHAProxy(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "iscsi/target":
		if err := runISCSITarget(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "db/postgres":
		if err := runPostgres(ctx, instance, args); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %v\n", err)
			os.Exit(1)
		}
	case "vm/instance":
		if err := runVM(ctx, instance, args); err != nil {
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
	Type  string   `json:"type"`
	Args  []string `json:"args,omitempty"`
	Index int      `json:"index"`
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
	body := os.Getenv("ECHO_BODY")
	if body == "" {
		body = cfg.Body
	}
	switch {
	case port != "":
		// env override wins (module.nix contract)
	case cfg.Port > 0:
		port = fmt.Sprintf("%d", int(cfg.Port))
	default:
		port = "0" // ephemeral: multiple replicas may share a node
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
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
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
