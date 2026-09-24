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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/expanse/expanse/internal/storage/drbd"
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

// cfgInt reads an integer config key, defaulting to def when absent or
// not positive.
func cfgInt(m map[string]any, k string, def int) int {
	if v, ok := m[k].(float64); ok && v > 0 {
		return int(v)
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

// voldevs decodes the bridge's "--voldev name=volID" args (bridge.go
// replicaSpec's raw-storage branch, PHASE-04-TASKS.md D3) into storage
// name → volume ID. Unlike mountPaths, the value is not a host path: a
// raw entry is never mounted (mount.Manager.attach never runs for one),
// so the only channel left to hand the workload is the bare volume ID,
// and it resolves its own device path locally (waitForPrimaryDevice)
// once this node actually holds that volume's DRBD Primary role.
func voldevs(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--voldev" {
			continue
		}
		if name, id, ok := strings.Cut(args[i+1], "="); ok {
			out[name] = id
		}
	}
	return out
}

// firstVoldev returns the lowest-sorted-name bound volume ID, or "" when
// none exist — same one-storage-entry convention as firstMount (D5: one
// LUN per iscsi/target instance).
func firstVoldev(m map[string]string) string {
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

// mkdirAllRetrying retries a MkdirAll-shaped call briefly rather than
// failing on the first error. mkdir is injected (matching waitForMount's
// own statter seam) so tests can fake transient failures without a real
// filesystem race.
func mkdirAllRetrying(mkdir func(string, os.FileMode) error, path string, perm os.FileMode, attempts int, interval time.Duration) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = mkdir(path, perm); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(interval)
		}
	}
	return err
}

// drbdStatuser abstracts drbd.Exec.Status for waitForPrimaryDevice's tests.
type drbdStatuser interface {
	Status(ctx context.Context, res string) (*drbd.Status, error)
}

// waitForPrimaryDevice blocks until volID's DRBD device is Primary on
// this node and reports its /dev/drbdN path, or returns an error once
// timeout elapses. Mirrors mount.Manager.device() (internal/storage/
// mount) — the same local, live-status resolution — reused here
// because a raw entry's workload is handed only the volume ID (D3),
// never a host path: there is nothing mounted for it to wait on.
func waitForPrimaryDevice(ctx context.Context, dr drbdStatuser, volID string, timeout, interval time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		st, err := dr.Status(ctx, volID)
		switch {
		case err != nil:
			lastErr = err
		case st.Role != drbd.RolePrimary:
			lastErr = fmt.Errorf("volume %s is not primary on this node yet (role=%s)", volID, st.Role)
		case len(st.Volumes) == 0:
			lastErr = fmt.Errorf("volume %s has no device yet", volID)
		default:
			return "/dev/drbd" + strconv.Itoa(st.Volumes[0].Minor), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("waitForPrimaryDevice %s: %w", volID, lastErr)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

// resetSMBEphemeralState wipes and recreates smbd's session/lock/
// liveness directories (locking.tdb, brlock.tdb, connections.tdb,
// serverid.tdb and friends) before every start. Those key their records
// by PID, meaningful only on the host that owns it — left in place
// across a failover to a different node (D3 puts them on the replicated
// volume so genuinely durable state, like the identity in privateDir,
// survives), a stale record is architecturally unsound to keep, since
// nothing on the new host can ever recognize the old PID as dead. This
// is what non-ctdb Samba HA setups do for exactly this reason.
// Investigated but not confirmed as the cause of a separate, still-open
// failover write bug (PHASE-03-TASKS.md Stream B2) -- kept regardless,
// since carrying node-local PID state across a failover is wrong on its
// own terms. privateDir is deliberately not passed here — the caller
// creates it separately.
func resetSMBEphemeralState(dirs ...string) error {
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	return nil
}

// resolveBin finds bin's real, symlink-followed location. NixOS's
// merged system profile (/run/current-system/sw/bin/<name>) is itself
// a symlink into the store; LookPath alone stops there; EvalSymlinks
// follows it the rest of the way. Some upstream binaries (postgres's
// find_my_exec, PHASE-05-TASKS.md Stream A X1) derive their own
// install prefix (here, where to find share/postgresql/postgres.bki)
// straight from argv[0] by stripping its last path components, with
// no readlink of their own — a profile-symlink path resolves to the
// profile's own layout, not the package's, unless this already handed
// them the real store path.
func resolveBin(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real, nil
	}
	return path, nil
}

// execWorkload runs a binary-backed workload. SIGTERM via ctx is the
// reconciler's deliberate stop, so a ctx-cancelled exit is not an
// error.
func execWorkload(ctx context.Context, bin string, argv []string, extraEnv ...string) error {
	path, err := resolveBin(bin)
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
	if err := resetSMBEphemeralState(lockDir, dbDir, cacheDir, pidDir, logDir, rpcDir); err != nil {
		return err
	}
	if err := os.MkdirAll(privateDir, 0o750); err != nil {
		return err
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
		// force user/force group: pins every guest connection to one
		// fixed identity rather than letting Samba resolve it fresh per
		// session (nixos.wiki's own canonical guest-share example sets
		// both). Investigated but not confirmed as the cause of a
		// separate, still-open failover write bug (PHASE-03-TASKS.md
		// Stream B2) -- kept because it's correct practice regardless.
		guestLines = "guest ok = yes\nmap to guest = Bad User\nguest account = nobody\n" +
			"force user = nobody\nforce group = nogroup\n"
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
  # This block's whole state directory (below) lives on the replicated
  # volume, by design (D3): a write samba hasn't actually fsynced to the
  # underlying block device never reaches DRBD, so a hard crash can lose
  # it outright, not just delay it -- including smbd's OWN identity/
  # session state (passdb.tdb, secrets.tdb), not only share content.
  # strict sync + sync always make every write durable before it's
  # acked, matching this project's durability bar everywhere else
  # (vol_durability's fsync ledger, fill_paced's conv=fsync).
  strict sync = yes
  sync always = yes
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
  log level = 3

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

// runISCSITarget serves iscsi/target (PHASE-04-TASKS.md Stream B):
// exports a raw DRBD-backed volume as an iSCSI LUN through LIO, driven
// by exec'ing targetcli-fb's non-interactive CLI form (one command per
// invocation, exits immediately — not its interactive shell, and not a
// second language in the runtime path) — the same "exec upstream
// tooling" shape every other binary-backed workload here uses. SINGLETON
// plus P12 colocation (D1, revised; Stream A's D2/D3) already guarantees
// this instance only ever runs on the node holding the volume's DRBD
// Primary — the one node the raw device is actually openable on
// (nix/tests/iscsi-lio-drbd-secondary-probe.nix). Unlike smbd, LIO has
// no long-running userspace daemon of its own: once configfs is set up,
// the kernel serves I/O directly, so this workload's job is to set that
// state up, then block until told to stop and tear it down again.
func runISCSITarget(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	volID := firstVoldev(voldevs(args))
	if volID == "" {
		return fmt.Errorf("iscsi/target: no bound storage volume yet")
	}
	dev, err := waitForPrimaryDevice(ctx, drbd.New(), volID, 30*time.Second, 500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("iscsi/target: %w", err)
	}

	iqn := cfgStr(cfg, "iqn")
	if iqn == "" {
		iqn = defaultIQN(instance)
	}
	wwn := cfgStr(cfg, "wwn")
	if wwn == "" {
		wwn = defaultWWN(instance)
	}
	port := cfgPort(cfg)
	if port == "0" {
		port = "3260"
	}

	lio := &lioTarget{
		backstore:    "expblk-" + sanitizeID(instance),
		iqn:          iqn,
		wwn:          wwn,
		device:       dev,
		port:         port,
		chapUser:     cfgStr(cfg, "chapUser"),
		chapPassword: cfgStr(cfg, "chapPassword"),
	}
	// Idempotent: a restart on the same node (Restart=on-failure) finds
	// its own prior objects still live in the kernel's configfs tree —
	// clear them before recreating, the same "wipe and recreate before
	// every start" philosophy resetSMBEphemeralState uses for its tdbs.
	lio.teardown(ctx)
	if err := lio.setup(ctx); err != nil {
		// setup() can fail partway through (e.g. the backstore was
		// created but the portal step after it failed) — leaving that
		// partial state behind would hold the raw device open in the
		// kernel indefinitely (target_core_iblock's own open, not tied
		// to this short-lived process), blocking every future DRBD
		// demotion on this node until something notices and cleans up
		// by hand. Reproduced directly: a transient setup failure here
		// left /dev/drbd0 open long after this process had exited,
		// and volume.stepDown kept failing ("Device is held open by
		// someone") on every later attempt to demote this node.
		lio.teardown(context.Background())
		return fmt.Errorf("iscsi/target: %w", err)
	}
	fmt.Printf("expanse-block-run: iscsi target %s exporting %s on :%s\n", iqn, dev, port)

	<-ctx.Done()
	// A demotion needs this node's LIO to fully release the device
	// before DRBD can demote it out from under a still-open backstore —
	// the same reason share/smb's own state lives where a promotion/
	// demotion can see it, applied here to a kernel-held fd instead of
	// a file. Best-effort: ctx is already done, so a fresh background
	// context carries the teardown itself.
	lio.teardown(context.Background())
	return nil
}

// lioTarget is one iscsi/target instance's LIO configfs state: a
// backstore, a target IQN with one TPG/portal, and the LUN linking them.
type lioTarget struct {
	backstore    string
	iqn          string
	wwn          string
	device       string
	port         string
	chapUser     string
	chapPassword string
}

// targetcli runs one non-interactive targetcli command (its documented
// CLI form: `targetcli <path> <command> [args...]` runs and exits, no
// shell). CombinedOutput folds targetcli's own error text into the
// returned error for diagnosability.
func targetcli(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "targetcli", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("targetcli %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setup creates this instance's backstore, target, portal and LUN.
func (l *lioTarget) setup(ctx context.Context) error {
	if err := targetcli(ctx, "/backstores/block", "create", "name="+l.backstore, "dev="+l.device, "wwn="+l.wwn); err != nil {
		return err
	}
	if err := targetcli(ctx, "/iscsi", "create", l.iqn); err != nil {
		return err
	}
	tpg := "/iscsi/" + l.iqn + "/tpg1"
	// No default portal to remove first: `/iscsi create` does not
	// auto-create one on this targetcli-fb version (confirmed directly —
	// a `portals delete 0.0.0.0 3260` here always failed with "No such
	// NetworkPortal in configfs"), unlike the reference declarative
	// services.target config some docs show. teardown() below always
	// deletes the whole target object first, so there is never a stale
	// portal left over to collide with this create either.
	if err := targetcli(ctx, tpg+"/portals", "create", "0.0.0.0", l.port); err != nil {
		return err
	}
	if err := targetcli(ctx, tpg+"/luns", "create", "/backstores/block/"+l.backstore); err != nil {
		return err
	}
	// generate_node_acls + demo_mode_write_protect=0: any initiator may
	// log in and get read-write access, the same "guest ok" posture
	// share/smb's guestOk default uses — gated on CHAP instead when a
	// chapUser is configured (schema.json), never on a pre-registered
	// initiator ACL (this phase has no ACL-provisioning story, matching
	// share/smb's validUsers scope).
	auth := "authentication=0"
	if l.chapUser != "" {
		auth = "authentication=1"
	}
	if err := targetcli(ctx, tpg, "set", "attribute", auth, "generate_node_acls=1", "demo_mode_write_protect=0"); err != nil {
		return err
	}
	if l.chapUser != "" {
		if err := targetcli(ctx, tpg, "set", "auth", "userid="+l.chapUser, "password="+l.chapPassword); err != nil {
			return err
		}
	}
	return nil
}

// teardown removes this instance's target and backstore. Errors are
// deliberately discarded: called both defensively before setup (nothing
// to remove yet, on a node that never ran this instance before) and on
// shutdown (best-effort release).
func (l *lioTarget) teardown(ctx context.Context) {
	_ = targetcli(ctx, "/iscsi", "delete", l.iqn)
	_ = targetcli(ctx, "/backstores/block", "delete", l.backstore)
}

// defaultIQN derives a stable target IQN from the block instance's own
// name (D4): identical on every node the SINGLETON replica ever lands
// on, since it depends on nothing node-specific — no identity needs to
// be minted once and persisted anywhere, which a raw LUN has no room
// for in the first place (D4's PR-state note makes the same argument).
func defaultIQN(instance string) string {
	return "iqn.2026-09.io.expanse:" + sanitizeID(instance)
}

// defaultWWN derives a stable NAA backstore WWN the same deterministic
// way: a hash of the instance name, formatted exactly as LIO's own
// randomly-generated default WWNs are (naa.5<IEEE-OUI><serial>, 16 hex
// digits total — NAA format code 5, IEEE Registered, 64-bit) so nothing
// downstream needs to treat it specially for being derived rather than
// random.
func defaultWWN(instance string) string {
	sum := sha256.Sum256([]byte("expanse-iscsi-wwn:" + instance))
	return "naa.5" + hex.EncodeToString(sum[:])[:15]
}

// sanitizeID lowercases and replaces anything outside IQN/backstore-safe
// characters (alnum, dash, dot) with a dash.
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
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

// pgBootstrapTimeout bounds how long a first-ever start waits for the
// election controller's (internal/blocks/pgha) role decision before
// giving up — mirrors the same budget nix/blocks/db/postgres/module.nix
// documents as the production deployment contract's own bootstrap
// script. That script is not actually exercised by any code path today
// (module.nix is nix-build-only — every other shipped block type routes
// through this same expanse-block-run dispatch instead, per main.go's
// switch); this is that port, kept in step with module.nix by hand.
const pgBootstrapTimeout = 120 * time.Second

// runPostgres serves db/postgres (PHASE-05-TASKS.md Stream A, X1). On a
// first-ever start (no PGDATA yet) it waits for the lease-gated
// election controller's role file and either initdb's a fresh primary
// or pg_basebackups from the elected one; a restart with PGDATA already
// initialized skips straight to exec — postgres remembers its own role
// via standby.signal's presence, and promotion afterward is pg_promote()
// (D4), not a restart.
//
// Runs under a fixed, non-root uid (pgha.StaticUID, wired via
// internal/blocks/wire/bridge.go's Spec.StaticUID) rather than
// DynamicUser: postgres refuses outright to run as uid 0, and
// DynamicUser mints a fresh uid on every unit start rather than once
// per replica, which the X1 VM test found left pgdata's ownership
// unable to survive a single Restart=on-failure cycle at all.
func runPostgres(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	mountPath := firstMount(mountPaths(args))
	if mountPath == "" {
		return fmt.Errorf("db/postgres: no bound storage mount yet")
	}
	if err := waitForMount(mountPath, 30*time.Second, 500*time.Millisecond, realStat); err != nil {
		return fmt.Errorf("db/postgres: %w", err)
	}

	port := cfgPort(cfg)
	if port == "0" {
		port = "5432"
	}
	database := cfgStr(cfg, "database")
	if database == "" {
		database = "app"
	}
	sharedBuffers := cfgStr(cfg, "sharedBuffers")
	if sharedBuffers == "" {
		sharedBuffers = "256MB"
	}
	maxWalSenders := cfgInt(cfg, "maxWalSenders", 10)
	maxReplicationSlots := cfgInt(cfg, "maxReplicationSlots", 10)
	replPassword := cfgStr(cfg, "replicationPassword")
	superPassword := cfgStr(cfg, "superuserPassword")
	if replPassword == "" || superPassword == "" {
		return fmt.Errorf("db/postgres: replicationPassword and superuserPassword are required")
	}

	stateDir := filepath.Join(mountPath, ".expanse-postgres")
	sockDir := filepath.Join(stateDir, "sock")
	pgdata := filepath.Join(mountPath, "pgdata")
	// mount.Manager.attach chmods a fresh mount world-writable in a
	// separate step right after the mount syscall itself, not
	// atomically with it (PHASE-05-TASKS.md Stream A X1) — a brief
	// retry tolerates waking from waitForMount in that narrow gap,
	// rather than failing permanently on a transient permission denied.
	if err := mkdirAllRetrying(os.MkdirAll, sockDir, 0o770, 10, 500*time.Millisecond); err != nil {
		return fmt.Errorf("db/postgres: %w", err)
	}

	if _, err := os.Stat(filepath.Join(pgdata, "PG_VERSION")); err != nil {
		slot := "expanse_" + sanitizeID(instance)
		if err := bootstrapPostgres(ctx, pgdata, filepath.Join(stateDir, "role"), sockDir,
			port, database, replPassword, superPassword, slot,
			sharedBuffers, maxWalSenders, maxReplicationSlots); err != nil {
			return fmt.Errorf("db/postgres: %w", err)
		}
	}

	fmt.Printf("expanse-block-run: postgres serving on :%s (pgdata=%s)\n", port, pgdata)
	return execWorkload(ctx, "postgres", []string{"-D", pgdata})
}

// waitForPGRole polls roleFile until pgha's election controller has
// written a decision, or timeout elapses. kind is "primary" or
// "replica"; host/peerport are set only for "replica" (pgha.go's
// "replica <host> <port>\n" format — the same "read -r kind host
// peerport" split module.nix's bootstrap script does on whitespace).
func waitForPGRole(ctx context.Context, roleFile string, timeout, interval time.Duration) (kind, host, peerport string, err error) {
	deadline := time.Now().Add(timeout)
	for {
		if data, rerr := os.ReadFile(roleFile); rerr == nil {
			if fields := strings.Fields(string(data)); len(fields) > 0 {
				kind = fields[0]
				if len(fields) >= 3 {
					host, peerport = fields[1], fields[2]
				}
				return kind, host, peerport, nil
			}
		}
		if time.Now().After(deadline) {
			return "", "", "", fmt.Errorf("no role at %s after %s", roleFile, timeout)
		}
		select {
		case <-ctx.Done():
			return "", "", "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

// pgHBAConf is pg_hba.conf's content for every db/postgres replica:
// local admin access (the controller, over the unix socket) is trust,
// everything else must present a password. Not restricted to the
// WireGuard overlay CIDR (an earlier, unverified assumption corrected
// by the X1 VM test): cross-node replication/client traffic (both the
// LB's own backend dialing, internal/agent/lb.go's lookupNodeIP, and
// this election controller's ResolveAddr, internal/blocks/pgha) both
// resolve a node's Raft/API advertise address, not an overlay address
// — an address this package has no fixed, predictable CIDR for at
// config-generation time, since it depends entirely on how the cluster
// was actually deployed. The password (scram-sha-256) is the real
// security boundary here, not a network-level restriction that does
// not match how this project's own node-to-node dialing works.
const pgHBAConf = `local   all             all                                     trust
host    replication     replicator      0.0.0.0/0               scram-sha-256
host    all             all             0.0.0.0/0               scram-sha-256
`

// writePGConf (re)writes pgdata's postgresql.conf and pg_hba.conf.
// Unconditional on every bootstrap, primary or replica: pg_basebackup's
// plain-format copy carries the PRIMARY's postgresql.conf into a fresh
// replica's pgdata too, so this replica's own port/socket-dir/hba
// settings must overwrite it, not just supply it once.
func writePGConf(pgdata, sockDir, port, sharedBuffers string, maxWalSenders, maxReplicationSlots int) error {
	if err := os.WriteFile(filepath.Join(pgdata, "pg_hba.conf"), []byte(pgHBAConf), 0o600); err != nil {
		return err
	}
	conf := fmt.Sprintf(`listen_addresses = '0.0.0.0'
port = %s
unix_socket_directories = '%s'
shared_buffers = %s
wal_level = replica
hot_standby = on
max_wal_senders = %d
max_replication_slots = %d
# Required for pg_rewind (PHASE-05-TASKS.md X4) -- a postmaster-context
# GUC, cheaper to set once at bootstrap than rediscover the need for it
# in Stream C.
wal_log_hints = on
`, port, sockDir, sharedBuffers, maxWalSenders, maxReplicationSlots)
	return os.WriteFile(filepath.Join(pgdata, "postgresql.conf"), []byte(conf), 0o600)
}

// pgCmd runs one short postgres-toolchain command to completion,
// wrapping its combined output into any error for diagnosability — the
// same shape targetcli() already uses for iscsi/target's one-shot setup
// commands.
//
// Resolves bin via resolveBin first (matching execWorkload's own
// pattern) rather than handing the bare name straight to
// exec.CommandContext: found via the X1 VM test, initdb's nixpkgs
// wrapper re-execs the real binary with --inherit-argv0, so whatever
// argv[0] this process set is what the real initdb sees too, and
// postgres's own find_my_exec() derives its install prefix from
// argv[0] by stripping path components — with no readlink of its own,
// so a bare name (CommandContext leaves argv[0] bare when only handed
// one; only cmd.Path gets resolved) or even a resolved-but-still-a-
// symlink path (LookPath alone stops at /run/current-system/sw/bin/
// initdb, the merged profile's own symlink) both derive the profile's
// layout instead of the real package's, missing
// share/postgresql/postgres.bki either way.
func pgCmd(ctx context.Context, env []string, stdin, bin string, args ...string) error {
	path, err := resolveBin(bin)
	if err != nil {
		return fmt.Errorf("block runtime: %s not found in PATH (ship the package): %w", bin, err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	// TEMPORARY diagnostic (PHASE-05-TASKS.md Stream A X1 VM test):
	// pgCmd previously only surfaced output on failure, but a run
	// showed a primary's CREATE ROLE/CREATE DATABASE step neither
	// crash the unit (no error) nor leave a usable database/role
	// behind -- printing on success too until that's understood. psql's
	// own output here is command tags ("CREATE ROLE") or error text,
	// never the SQL/password themselves.
	if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
		fmt.Printf("expanse-block-run: %s output: %s\n", bin, trimmed)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// bootstrapPostgres runs the once-ever init sequence module.nix
// documents: wait for the controller's role decision, then either
// initdb a fresh primary (and create the replicator role + initial
// database) or pg_basebackup from the elected primary as a standby.
func bootstrapPostgres(ctx context.Context, pgdata, roleFile, sockDir, port, database, replPassword, superPassword, slot,
	sharedBuffers string, maxWalSenders, maxReplicationSlots int) error {
	kind, host, peerport, err := waitForPGRole(ctx, roleFile, pgBootstrapTimeout, time.Second)
	if err != nil {
		return err
	}

	switch kind {
	case "primary":
		pwFile, err := os.CreateTemp("", "expblk-pg-pwfile-*")
		if err != nil {
			return err
		}
		defer os.Remove(pwFile.Name())
		_, werr := pwFile.WriteString(superPassword)
		cerr := pwFile.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
		// TEMPORARY step-by-step diagnostic (PHASE-05-TASKS.md Stream A
		// X1 VM test): a run left the primary's postgres genuinely
		// serving connections (auth as the initdb superuser works) yet
		// neither the replicator role nor the database ever existed,
		// with no error/crash logged at all -- consistent with pg_ctl
		// -w start itself never returning even though the daemonized
		// postmaster it forked came up fine regardless. Bracketing each
		// step until that's confirmed.
		fmt.Println("expanse-block-run: bootstrapPostgres: running initdb")
		if err := pgCmd(ctx, nil, "", "initdb", "-D", pgdata, "--username=postgres", "--pwfile="+pwFile.Name()); err != nil {
			return err
		}
		if err := writePGConf(pgdata, sockDir, port, sharedBuffers, maxWalSenders, maxReplicationSlots); err != nil {
			return err
		}
		fmt.Println("expanse-block-run: bootstrapPostgres: starting postgres for bootstrap SQL")
		if err := pgCmd(ctx, nil, "", "pg_ctl", "-D", pgdata, "-w", "start"); err != nil {
			return err
		}
		fmt.Println("expanse-block-run: bootstrapPostgres: pg_ctl start returned; running CREATE ROLE/CREATE DATABASE")
		sql := "CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD :'pass';\nCREATE DATABASE :\"dbname\";\n"
		createErr := pgCmd(ctx, nil, sql, "psql", "-h", sockDir, "-p", port, "-U", "postgres",
			"-v", "ON_ERROR_STOP=1", "-v", "pass="+replPassword, "-v", "dbname="+database)
		fmt.Printf("expanse-block-run: bootstrapPostgres: CREATE ROLE/CREATE DATABASE returned: %v\n", createErr)
		if stopErr := pgCmd(ctx, nil, "", "pg_ctl", "-D", pgdata, "-w", "stop"); stopErr != nil && createErr == nil {
			return stopErr
		}
		fmt.Println("expanse-block-run: bootstrapPostgres: primary bootstrap complete")
		return createErr
	case "replica":
		env := []string{"PGPASSWORD=" + replPassword}
		if err := pgCmd(ctx, env, "", "pg_basebackup",
			"-h", host, "-p", peerport, "-U", "replicator",
			"-D", pgdata, "-Fp", "-Xs", "-R", "-C", "-S", slot); err != nil {
			return err
		}
		return writePGConf(pgdata, sockDir, port, sharedBuffers, maxWalSenders, maxReplicationSlots)
	default:
		return fmt.Errorf("unrecognised role kind %q in %s", kind, roleFile)
	}
}
