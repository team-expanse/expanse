// Shipped-block workloads for expanse-block-run (PHASE04.md §3.3/§6,
// T24). Each shipped block type gets a real runtime so the §8
// block-catalog VM test can assert functionality (nginx HTTP 200,
// redis-cli ping, ollama /api/tags, node-exporter metrics, static-site
// index, echo body).
//
// Two shapes:
//   - binary-backed (nginx, redis, node_exporter, ollama): exec the
//     upstream binary from PATH (the deployment image ships it — the
//     module.nix contract names the package); config maps to flags or
//     a generated config file under /tmp (PrivateTmp in the unit).
//   - native (static-site, echo): implemented directly in Go.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// cfgMap decodes the --config JSON argument into a generic map.
func cfgMap(args []string) (map[string]any, error) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--config" {
			var m map[string]any
			if err := json.Unmarshal([]byte(args[i+1]), &m); err != nil {
				return nil, fmt.Errorf("bad --config: %w", err)
			}
			return m, nil
		}
	}
	return map[string]any{}, nil
}

func cfgStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func cfgPort(m map[string]any) string {
	if v, ok := m["port"].(float64); ok && v > 0 {
		return fmt.Sprintf("%d", int(v))
	}
	return "0" // ephemeral: replicas may share a node
}

// execWorkload runs a binary-backed workload. SIGTERM via ctx is the
// reconciler's deliberate stop, so a ctx-cancelled exit is not an
// error.
func execWorkload(ctx context.Context, bin string, argv []string, extraEnv ...string) error {
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("block runtime: %s not found in PATH (ship the package): %w", bin, err)
	}
	cmd := exec.CommandContext(ctx, path, argv...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil // deliberate stop
	}
	return err
}

// runNginx serves web/nginx: generate a minimal vhost config and exec
// the upstream nginx binary (daemon off, foreground).
func runNginx(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	port := cfgPort(cfg)
	root := cfgStr(cfg, "root")
	if root == "" {
		root = "/tmp/expblk-" + instance + "-www"
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	index := filepath.Join(root, "index.html")
	if _, err := os.Stat(index); err != nil {
		if err := os.WriteFile(index, []byte("<html><body>expanse nginx block</body></html>\n"), 0o644); err != nil {
			return err
		}
	}
	conf := "/tmp/expblk-" + instance + ".conf"
	body := fmt.Sprintf(`
worker_processes 1;
pid /tmp/expblk-%s.pid;
error_log /tmp/expblk-%s-error.log;
events { }
http {
  access_log /dev/null;
  server {
    listen %s;
    server_name %s;
    root %s;
    index index.html;
  }
}
`, instance, instance, port, cfgStr(cfg, "serverName"), root)
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		return err
	}
	fmt.Printf("expanse-block-run: nginx serving on :%s\n", port)
	return execWorkload(ctx, "nginx", []string{"-c", conf, "-e", "/tmp/expblk-" + instance + "-error.log", "-g", "daemon off;"})
}

// runRedis serves db/redis: generate a redis.conf and exec the
// upstream redis-server binary.
func runRedis(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	port := cfgPort(cfg)
	maxMem := cfgStr(cfg, "maxMemory")
	if maxMem == "" {
		maxMem = "256mb"
	}
	dir := "/tmp/expblk-" + instance + "-data"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	conf := "/tmp/expblk-" + instance + ".conf"
	body := fmt.Sprintf(`
port %s
bind 0.0.0.0
dir %s
maxmemory %s
maxmemory-policy %s
`, port, dir, maxMem, cfgStr(cfg, "maxMemoryPolicy"))
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		return err
	}
	fmt.Printf("expanse-block-run: redis serving on :%s\n", port)
	return execWorkload(ctx, "redis-server", []string{conf})
}

// runNodeExporter serves monitor/node-exporter: exec the upstream
// node_exporter binary on the configured port.
func runNodeExporter(ctx context.Context, _ string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	port := cfgPort(cfg)
	path := cfgStr(cfg, "webTelemetryPath")
	if path == "" {
		path = "/metrics"
	}
	fmt.Printf("expanse-block-run: node_exporter serving on :%s\n", port)
	return execWorkload(ctx, "node_exporter", []string{
		"--web.listen-address=0.0.0.0:" + port,
		"--web.telemetry-path=" + path,
	})
}

// runOllama serves ai/ollama: exec the upstream ollama binary in serve
// mode bound to the configured port.
func runOllama(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	port := cfgPort(cfg)
	models := cfgStr(cfg, "models")
	if models == "" {
		models = "/tmp/expblk-" + instance + "-models"
	}
	if err := os.MkdirAll(models, 0o755); err != nil {
		return err
	}
	// ollama requires $HOME (it refuses to start without one).
	home := "/tmp/expblk-" + instance + "-home"
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	fmt.Printf("expanse-block-run: ollama serving on :%s\n", port)
	return execWorkload(ctx, "ollama", []string{
		"serve",
		// serve has no host flag in older versions; the env var is the
		// supported knob.
	}, "OLLAMA_HOST=0.0.0.0:"+port, "OLLAMA_MODELS="+models, "HOME="+home)
}

// runStaticSite serves web/static-site natively: the configured index
// HTML is written into the block's volume mount (spec.storage — Phase
// 06 wires the volume; until then a local dir) and served.
func runStaticSite(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	port := cfgPort(cfg)
	dir := "/tmp/expblk-" + instance + "-site"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	idx := cfgStr(cfg, "index")
	if idx == "" {
		idx = "<html><body>expanse static site</body></html>"
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(idx), 0o644); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           http.FileServer(http.Dir(dir)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	fmt.Printf("expanse-block-run: static-site serving on :%s\n", port)
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
