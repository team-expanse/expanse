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
	"sort"
	"strings"
	"syscall"
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

// cfgBool reads a boolean config key, defaulting to def when absent.
func cfgBool(m map[string]any, k string, def bool) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return def
}

// mountPaths decodes the bridge's "--mount name=path" args (bridge.go
// replicaSpec) into storage name → host mount path. It is the only
// channel a storage-bound workload has for its own mountPath: --config
// carries just the user's schema-validated spec.config.
func mountPaths(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--mount" {
			continue
		}
		if name, path, ok := strings.Cut(args[i+1], "="); ok {
			out[name] = path
		}
	}
	return out
}

// firstMount returns the lowest-sorted-name mount path, or "" when none
// exist. Share blocks bind exactly one storage entry (D4), so which one
// never matters in practice — sorting only makes the choice deterministic
// when there happens to be more than one.
func firstMount(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return m[names[0]]
}

// statter abstracts syscall.Stat for waitForMount's tests.
type statter func(path string, st *syscall.Stat_t) error

func realStat(path string, st *syscall.Stat_t) error { return syscall.Stat(path, st) }

// waitForMount blocks until path is a real, distinct mounted filesystem
// (a different st_dev than its parent directory), or returns an error
// once timeout elapses. The volume-mount reconcile resource
// (internal/storage/mount) converges independently of — and can lag
// behind — the block-replica spec that names this path
// (internal/blocks/wire/bridge.go): a storage-bound workload that
// writes to mountPath before the real filesystem lands there writes to
// the plain pre-mount directory instead, which the mount then shadows,
// hiding everything just written. Reproduced directly: smbd's own
// state directory went missing exactly this way.
func waitForMount(path string, timeout, interval time.Duration, stat statter) error {
	deadline := time.Now().Add(timeout)
	parent := filepath.Dir(path)
	var lastErr error
	for {
		var pst, cst syscall.Stat_t
		switch {
		case stat(parent, &pst) != nil:
			lastErr = fmt.Errorf("stat %s: not yet available", parent)
		case stat(path, &cst) != nil:
			lastErr = fmt.Errorf("stat %s: not yet available", path)
		case cst.Dev != pst.Dev:
			return nil
		default:
			lastErr = fmt.Errorf("%s is not yet a distinct mount from %s", path, parent)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waitForMount %s: %w", path, lastErr)
		}
		time.Sleep(interval)
	}
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

// runSMB serves share/smb (PHASE-03-TASKS.md Stream B1): generate a
// per-instance smb.conf and exec the upstream smbd. All of smbd's own
// session state (D3: locking.tdb, brlock.tdb, connections.tdb and
// friends) is relocated onto the bound volume so it fails over with the
// data instead of resetting on every promotion — the same state
// directory layout the share/smb module.nix contract documents.
// mountPath here is the volume's real HOST path (bridge.go), the one
// path the block's sandbox actually exposes — not the user's declared
// spec.storage[].mountPath (that governs the separate host-level bind
// mount §4.7 performs; see bridge.go's replicaSpec for why the two
// differ). The unit runs RunAsRoot (bridge.go): smbd setuid()s to the
// connecting or guest user per session, a capability no unprivileged
// uid can hold.
func runSMB(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	mountPath := firstMount(mountPaths(args))
	if mountPath == "" {
		return fmt.Errorf("share/smb: no bound storage mount yet")
	}
	// The bridge names this path as soon as the volume record exists,
	// not once the host-level mount actually completes (waitForMount's
	// doc comment) — never create smbd's state here before that lands.
	if err := waitForMount(mountPath, 30*time.Second, 500*time.Millisecond, realStat); err != nil {
		return fmt.Errorf("share/smb: %w", err)
	}
	shareName := cfgStr(cfg, "shareName")
	if shareName == "" {
		shareName = "share"
	}
	relPath := cfgStr(cfg, "path")
	if relPath == "" {
		relPath = "."
	}
	sharePath := filepath.Join(mountPath, relPath)
	stateDir := filepath.Join(mountPath, ".smb-state")
	lockDir := filepath.Join(stateDir, "lock")
	dbDir := filepath.Join(stateDir, "state")
	cacheDir := filepath.Join(stateDir, "cache")
	privateDir := filepath.Join(stateDir, "private")
	pidDir := filepath.Join(stateDir, "run")
	logDir := filepath.Join(stateDir, "log")
	// smbd's internal RPC named-pipe directory — a separate smb.conf
	// parameter from all the ones above, defaulting to the hardcoded
	// /var/run/samba/ncalrpc if left unset, which does not exist (and
	// cannot: ProtectSystem=strict) here.
	rpcDir := filepath.Join(stateDir, "ncalrpc")
	for _, d := range []string{lockDir, dbDir, cacheDir, privateDir, pidDir, logDir, rpcDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	// Share content is a different trust boundary from smbd's own state:
	// a guest session runs as "nobody" (map to guest, below), which is
	// in neither root's user nor group and so cannot write under a
	// root-owned 0750 directory at all.
	sharePerm := os.FileMode(0o750)
	guestOk := cfgBool(cfg, "guestOk", true)
	if guestOk {
		sharePerm = 0o777
	}
	if err := os.MkdirAll(sharePath, sharePerm); err != nil {
		return err
	}
	if err := os.Chmod(sharePath, sharePerm); err != nil {
		return err
	}

	port := cfgPort(cfg)
	if port == "0" {
		port = "445"
	}
	guestLines := "guest ok = no\n"
	if guestOk {
		guestLines = "guest ok = yes\nmap to guest = Bad User\nguest account = nobody\n"
	}
	validUsersLine := ""
	if u := cfgStr(cfg, "validUsers"); u != "" {
		validUsersLine = "valid users = " + u + "\n"
	}
	netbios := instance
	if len(netbios) > 15 {
		netbios = netbios[:15]
	}
	readOnly := "no"
	if cfgBool(cfg, "readOnly", false) {
		readOnly = "yes"
	}
	browseable := "yes"
	if !cfgBool(cfg, "browseable", true) {
		browseable = "no"
	}

	conf := filepath.Join(stateDir, "smb.conf")
	body := fmt.Sprintf(`[global]
  netbios name = %s
  workgroup = WORKGROUP
  security = user
  server min protocol = SMB3
  smb ports = %s
  # The sandbox's RestrictAddressFamilies (AF_UNIX/AF_INET/AF_INET6 only,
  # nix/modules/agent.nix) has no AF_NETLINK, which smbd otherwise needs
  # to auto-enumerate interfaces — it refuses to start at all without
  # this line ("Could not determine network interfaces"). "0.0.0.0/0"
  # covers every address without asking the kernel to list any.
  interfaces = 0.0.0.0/0
  bind interfaces only = no
  disable spoolss = yes
  load printers = no
  printing = bsd
  printcap name = /dev/null
  lock directory = %s
  state directory = %s
  cache directory = %s
  private dir = %s
  pid directory = %s
  ncalrpc dir = %s
  log file = %s/log.%%m
  log level = 1

[%s]
  path = %s
  read only = %s
  browseable = %s
  %s%s`,
		netbios, port, lockDir, dbDir, cacheDir, privateDir, pidDir, rpcDir, logDir,
		shareName, sharePath, readOnly, browseable, guestLines, validUsersLine)
	if err := os.WriteFile(conf, []byte(body), 0o640); err != nil {
		return err
	}
	fmt.Printf("expanse-block-run: smb serving %q on :%s\n", shareName, port)
	// -l: smbd's own startup logging (before smb.conf is even parsed)
	// uses this, not the "log file" directive above — without it, the
	// very first log line falls back to the compiled-in /var/log/samba,
	// which does not exist (and cannot: ProtectSystem=strict) here.
	// -S: send smbd's own debug/log output to stdout, the journald
	// convention every other block follows — without it, a startup
	// failure after config parsing is silent (samba logs to the log
	// file, never stderr, once past the bootstrap stage -l covers).
	return execWorkload(ctx, "smbd", []string{"--foreground", "--no-process-group", "--debug-stdout", "-l", logDir, "-s", conf})
}

// runWhoami serves web/whoami: an in-process HTTP server that reports
// the replica index (from the per-replica spec the agent wrote) — the
// load-balancer distribution tests' distinguishing backend. Binds all
// interfaces so cross-node backends are reachable through the LAN.
func runWhoami(ctx context.Context, index int, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	port := cfgPort(cfg)
	if port == "0" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	greeting := ""
	if v, ok := cfg["greeting"].(string); ok {
		greeting = v
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The LB tests assert X-Forwarded-For propagation (G5.8); the
		// header mirror keeps the body shape ("replica-N") stable for
		// the distribution assertions.
		w.Header().Set("X-Seen-XFF", r.Header.Get("X-Forwarded-For"))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fmt.Sprintf("%sreplica-%d\n", greeting, index)))
	})
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	fmt.Printf("expanse-block-run: whoami(replica-%d) serving on :%s\n", index, port)
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
