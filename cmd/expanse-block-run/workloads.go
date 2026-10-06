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
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/expanse/expanse/internal/blocks/vmready"
	"github.com/expanse/expanse/internal/network/vip"
	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/vishvananda/netlink"
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
		return nil //nolint:nilerr // deliberate stop
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
	// `/iscsi create` adds a default portal on [::0]:3260, which would hold the VIP's port on this
	// node. It is absent when 3260 was already taken, so a failed delete is fine.
	_ = targetcli(ctx, tpg+"/portals", "delete", "::0", "3260")
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

// pgSlotName derives a physical replication slot name from a replica's
// instance string. Postgres slot names must match [a-z0-9_]+ (X1 VM
// test: sanitizeID's own dash/dot allowance, fine for IQNs, is invalid
// here -- "expanse_default-pg-0" was rejected with "contains invalid
// character") -- '-' and '.' become '_'.
func pgSlotName(instance string) string {
	return "expanse_" + strings.NewReplacer("-", "_", ".", "_").Replace(sanitizeID(instance))
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
		_, _ = fmt.Fprintf(w, "%sreplica-%d\n", greeting, index)
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

// pgBootstrapTimeout bounds how long a first-ever start waits for the
// election controller's (internal/blocks/pgha) role decision before
// giving up — mirrors the same budget nix/blocks/db/postgres/module.nix
// documents as the production deployment contract's own bootstrap
// script. That script is not actually exercised by any code path today
// (module.nix is nix-build-only — every other shipped block type routes
// through this same expanse-block-run dispatch instead, per main.go's
// switch); this is that port, kept in step with module.nix by hand.
const pgBootstrapTimeout = 120 * time.Second

// pgBasebackupTimeout bounds one pg_basebackup attempt within
// bootstrapPostgres's replica case. Comfortably inside pgBootstrapTimeout
// itself only bounds waitForPGRole, not this step, so a stuck backup
// previously had no ceiling at all.
const pgBasebackupTimeout = 60 * time.Second

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

	roleFile := filepath.Join(stateDir, "role")
	if _, err := os.Stat(filepath.Join(pgdata, "PG_VERSION")); err != nil {
		slot := pgSlotName(instance)
		if err := bootstrapPostgres(ctx, pgdata, roleFile, sockDir,
			port, database, replPassword, superPassword, slot,
			sharedBuffers, maxWalSenders, maxReplicationSlots); err != nil {
			return fmt.Errorf("db/postgres: %w", err)
		}
	}

	// A demoted flag, not ctx cancellation: execWorkload treats a
	// ctx-cancelled exit as this process's own deliberate stop and
	// returns nil either way (its own doc comment explains why), which
	// would leave a demotion-triggered stop looking identical to a
	// clean shutdown -- exactly the case that must instead force a
	// non-zero return here, so systemd's Restart=on-failure actually
	// restarts the unit and re-bootstraps fresh (X4).
	var demoted atomic.Bool
	demoteDone := make(chan struct{})
	watching := isLocalPrimary(pgdata)
	if watching {
		go func() {
			watchForDemotion(ctx, roleFile, pgdata, pgDemotePollInterval, &demoted)
			close(demoteDone)
		}()
	}

	fmt.Printf("expanse-block-run: postgres serving on :%s (pgdata=%s)\n", port, pgdata)
	err = execWorkload(ctx, "postgres", []string{"-D", pgdata})
	if watching && demoted.Load() {
		// demoted.Store(true) happens (watchForDemotion's own code)
		// strictly before it ever signals postgres to stop, so by now
		// it is already set -- but execWorkload returns as soon as
		// postgres itself (its own direct child) exits, which -- found
		// running the X4 VM test -- consistently beats watchForDemotion's
		// own pg_ctl stop call finishing and reaching os.RemoveAll:
		// pg_ctl's own subprocess still has to notice postgres is gone
		// and return control to pgCmd afterward, strictly more steps
		// than execWorkload's direct wait on the same exit. Returning
		// here regardless would let this whole process exit -- killing
		// that goroutine -- often BEFORE it ever wipes pgdata, so every
		// restart just re-found the same never-actually-wiped diverged
		// primary forever. Only reached once a demotion is confirmed
		// already in progress, so this never delays an ordinary exit.
		select {
		case <-demoteDone:
		case <-time.After(10 * time.Second):
		}
		return fmt.Errorf("db/postgres: demoted to replica, restarting to re-clone from the new primary (X4)")
	}
	return err
}

// pgDemotePollInterval bounds how quickly a stale, rebooted-or-healed
// old primary notices pgha has demoted it (Stream C, X4). Comfortably
// under pgha.LeaseTTL (10s): the role file can only flip once pgha's own
// reclaim has already lost for real, so there is no failover-latency
// budget to race here, just a "don't sit diverged any longer than
// necessary" one.
const pgDemotePollInterval = 3 * time.Second

// isLocalPrimary reports whether pgdata's own on-disk state currently
// configures it as a primary rather than a streaming standby.
// standby.signal is postgres's own marker, written unconditionally by
// pg_basebackup's -R flag (bootstrapPostgres's replica case) — reading
// it directly is simpler and more trustworthy than tracking a bootstrap-
// time flag, since it reflects the actual cluster on disk regardless of
// which run of this process (or which bootstrap decision, possibly
// stale) put it there.
func isLocalPrimary(pgdata string) bool {
	_, err := os.Stat(filepath.Join(pgdata, "standby.signal"))
	return os.IsNotExist(err)
}

// roleFileSaysReplica reports whether roleFile's raw content is a
// "replica <host> <port>" decision, the parsing waitForPGRole itself
// already does, pulled out as a pure predicate for watchForDemotion.
func roleFileSaysReplica(content string) bool {
	fields := strings.Fields(content)
	return len(fields) > 0 && fields[0] == "replica"
}

// watchForDemotion runs for as long as this node's postgres started out
// primary, watching pgha's own role file for the flip to
// "replica <host> <port>" that means pgha's reclaim lost to a different,
// live node (internal/blocks/pgha's demoteIfLostToAnother) — this node
// crashed or was partitioned away for long enough that another replica
// already promoted for real (Stream C, X4).
//
// On that signal: stop this node's own postgres and wipe its now-
// diverged pgdata, so the unit's forced non-zero exit (runPostgres's own
// caller) makes systemd restart it into a fresh bootstrap — pg_basebackup
// from the new primary the role file already names, not a permanently
// diverged standalone primary or a manual pg_rewind.
func watchForDemotion(ctx context.Context, roleFile, pgdata string, interval time.Duration, demoted *atomic.Bool) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		content, err := os.ReadFile(roleFile)
		if err != nil || !roleFileSaysReplica(string(content)) {
			continue
		}
		demoted.Store(true)
		// Best effort: even a failed stop still gets the directory wiped
		// (postgres exiting on SIGTERM from execWorkload's own eventual
		// ctx-cancel teardown, or already dead, either way) and the
		// forced non-zero return still restarts the unit either way.
		_ = pgCmd(context.Background(), nil, "", "pg_ctl", "-D", pgdata, "-m", "fast", "-w", "stop")
		_ = os.RemoveAll(pgdata)
		return
	}
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
//
// sync gates synchronous_standby_names (X2's "no acknowledged
// transaction is lost": a commit does not return to the client until at
// least one standby has confirmed receipt, so whichever replica pgha
// promotes after the primary dies already has every write the client
// believes succeeded) -- it must be false for a brand new primary's
// very first start, the one bootstrapPostgres uses only to run CREATE
// ROLE replicator/CREATE DATABASE over a still-empty cluster: no
// standby can possibly be connected yet (none can even authenticate
// before that CREATE ROLE commits), so turning this on that early
// makes the CREATE ROLE statement itself wait forever for a standby
// that can never arrive -- a genuine deadlock, not just a slow start.
// Every other start (the primary's real, long-running one, and every
// replica's) passes true.
func writePGConf(pgdata, sockDir, port, sharedBuffers string, maxWalSenders, maxReplicationSlots int, sync bool) error {
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
	if sync {
		// "*" matches any connected standby by application_name, not a
		// specific one: with two standbys, ANY 1 tolerates either being
		// briefly unreachable (bootstrapping, restarting) without
		// blocking every write, at the cost of not guaranteeing the
		// OTHER standby also has every acked write -- tracked as a
		// known gap, not a byzantine-safe guarantee, per
		// PHASE-05-TASKS.md D1's R1 (measure, don't assume, against the
		// actual failover VM test).
		conf += "synchronous_standby_names = 'ANY 1 (*)'\n"
	}
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
// runVM serves vm/instance (PHASE-06-TASKS.md Stream A, X1): exec
// qemu-kvm directly against the bound raw volume as its disk (D3, the
// same --voldev/waitForPrimaryDevice channel runISCSITarget already
// uses) and a macvtap child of the node's own uplink as its network
// identity (D2, ARCHITECTURE.md A34), MAC pinned once from the instance
// name the same deterministic way defaultIQN/defaultWWN pin
// iscsi/target's identity — no libvirt (D1, A33 revised), the same
// "exec upstream tooling, drive its lifecycle over its own control
// socket" shape every other binary-backed workload here uses.
//
// A stop/demotion is a hard kill of the qemu-kvm child (ctx cancel ->
// exec's default SIGKILL): live migration and graceful guest quiesce
// are explicitly out of scope this phase (X5), and process death always
// closes qemu's own fd on the raw device, so — unlike LIO's kernel-held
// configfs backstore (lioTarget.teardown) — no separate release step is
// needed for DRBD to demote cleanly afterward.
func runVM(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}

	volID := firstVoldev(voldevs(args))
	if volID == "" {
		return fmt.Errorf("vm/instance: no bound storage volume yet")
	}
	dev, err := waitForPrimaryDevice(ctx, drbd.New(), volID, 30*time.Second, 500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("vm/instance: %w", err)
	}

	mac := cfgStr(cfg, "mac")
	if mac == "" {
		mac = defaultMAC(instance)
	}
	cpus := vmCPUs(args)
	memMiB := vmMemMiB(args)

	// EXPANSE_EXTERNAL_INTERFACE (nix/modules/agent.nix, from the agent's
	// own --external-interface): the same physical NIC the VIP holder
	// already announces on. Empty falls back to "auto" (the default
	// route's device) -- the right answer for a real deployment with a
	// gateway, but not for this project's own nixosTest harness, whose
	// flat-LAN test nodes carry no default route at all (only directly-
	// connected subnet routes), confirmed the hard way: vip.ResolveIface
	// returned "no default route interface for auto detection" until the
	// test explicitly set externalInterface, exactly as every VIP-using
	// block test already must.
	_, uplink, err := vip.ResolveIface(uplinkIfaceFromEnv(os.Getenv("EXPANSE_EXTERNAL_INTERFACE")))
	if err != nil {
		return fmt.Errorf("vm/instance: resolve uplink interface: %w", err)
	}
	// Guest readiness (vm-vsock-notify-probe.nix): the guest's systemd reports its boot over vsock.
	var watch *guestWatch
	switch mode := cfgStr(cfg, "guestReady"); mode {
	case "", "systemd":
		if w, err := startGuestWatch(instance); err != nil {
			publishGuestStatus(instance, "not ready: cannot watch the guest: "+err.Error())
		} else {
			watch = w
			defer w.close()
		}
	case "none":
	default:
		return fmt.Errorf("vm/instance: guestReady %q: want systemd or none", mode)
	}

	tapName := macvtapIfaceName(instance)
	tap, err := createMacvtap(uplink, tapName, mac)
	if err != nil {
		return fmt.Errorf("vm/instance: %w", err)
	}

	// Removed up front, matching createMacvtap's own idempotent "wipe
	// and recreate before every start" convention: a restart on the same
	// node (Restart=on-failure) finds its own prior socket file still on
	// disk even though the process behind it is long gone (PrivateTmp
	// keeps this unit's /tmp only for itself, but qemu's own bind() is
	// not guaranteed to unlink a stale path first).
	qmpSock := "/tmp/expblk-" + instance + "-qmp.sock"
	_ = os.Remove(qmpSock)

	qemuArgv := []string{
		"-enable-kvm", "-nodefaults", "-display", "none", "-no-reboot",
		"-m", strconv.Itoa(memMiB), "-smp", strconv.Itoa(cpus),
		"-drive", "file=" + dev + ",if=virtio,format=raw",
		// fd=3: the first (and only) entry in cmd.ExtraFiles below --
		// qemu itself never needs CAP_NET_ADMIN or any netlink access of
		// its own, since this already-root process did the one-time
		// privileged macvtap setup and hands the already-open fd down.
		"-netdev", "tap,id=net0,fd=3",
		"-device", "virtio-net-pci,netdev=net0,mac=" + mac,
		"-serial", "stdio",
		"-qmp", "unix:" + qmpSock + ",server,nowait",
	}
	// D4: getting a first bootable guest OS onto an empty raw volume is
	// documented, not built, this phase. Left unset (the production
	// default), the VMM boots from the attached disk the ordinary BIOS
	// way; bootKernel opts into direct-kernel-boot instead, the same
	// mechanism nix/tests/vm-d1-boot-probe.nix already measured working.
	if k := cfgStr(cfg, "bootKernel"); k != "" {
		qemuArgv = append(qemuArgv, "-kernel", k)
		if i := cfgStr(cfg, "bootInitrd"); i != "" {
			qemuArgv = append(qemuArgv, "-initrd", i)
		}
		if c := cfgStr(cfg, "bootCmdline"); c != "" {
			qemuArgv = append(qemuArgv, "-append", c)
		}
	}

	extra := []*os.File{tap} // fd 3
	if watch != nil {
		qemuArgv = append(qemuArgv, vmready.QEMUArgs(watch.cid, 4)...)
		extra = append(extra, watch.vhost) // fd 4
	}

	path, err := resolveBin("qemu-kvm")
	if err != nil {
		_ = tap.Close()
		_ = deleteLink(tapName)
		return fmt.Errorf("vm/instance: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, qemuArgv...)
	cmd.ExtraFiles = extra
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = tap.Close()
		_ = deleteLink(tapName)
		return fmt.Errorf("vm/instance: start qemu-kvm: %w", err)
	}
	// qemu has its own inherited copy of the fd (via fork/exec) as soon
	// as Start returns; closing our own copy here does not affect it.
	_ = tap.Close()
	fmt.Printf("expanse-block-run: vm %s booting %s (mac=%s cpus=%d memMiB=%d)\n", instance, dev, mac, cpus, memMiB)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	if watch != nil {
		go watch.serve(watchCtx, instance)
	} else if cfgStr(cfg, "guestReady") == "none" {
		publishGuestStatus(instance, vmready.ReadyPrefix+" not waited on (guestReady: none)")
	}
	err = cmd.Wait()
	_ = deleteLink(tapName)
	if ctx.Err() != nil {
		return nil //nolint:nilerr // deliberate stop
	}
	return err
}

// createMacvtap creates a bridge-mode macvtap child of uplink (D2,
// ARCHITECTURE.md A34), pins mac, brings it up, and opens its kernel
// tap character device — the fd qemu's own "-netdev tap,fd=N" takes
// directly. Idempotent, matching lioTarget's own "wipe and recreate
// before every start" convention: a restart on the same node finds its
// own prior macvtap child still present.
// createMacvtap creates a bridge-mode macvtap child of uplink by
// exec'ing iproute2's `ip link` directly — not vishvananda/netlink's
// own LinkAdd, despite that being this project's usual preference (VIP
// address management, internal/network/vip) for exactly this class of
// operation. Measured, not guessed: netlink.LinkAdd for a Macvtap link
// reproducibly failed with ERANGE ("numerical result out of range")
// against this project's own X1 VM test — persistently, not a boot-time
// race (30+ retries over 140s all failed identically) — while the exact
// same operation via `ip link add ... type macvtap mode bridge`,
// exactly nix/tests/vm-macvtap-probe.py's own already-proven sequence,
// is the one actually measured working. Same class of finding as D1's
// own cloud-hypervisor rejection: a real, reproducible incompatibility,
// not a config mistake — adopt the proven path instead of chasing it.
// macvtapIfaceName derives a stable, deterministic macvtap child
// interface name from the block instance's own name — identical on
// every failover, the same pattern defaultMAC/defaultIQN/defaultWWN
// already use. Linux kernel interface names are capped at IFNAMSIZ-1
// (15 bytes); a straightforward "mvtap-<instance>" prefix routinely
// blows past that for any realistic namespace/name/index combination
// (confirmed the hard way against the X1 VM test: "mvtap-default-
// vm1-0" is 19 characters, rejected outright — deterministically, not
// flaky, the length check fires on every single attempt). Hashing
// keeps the result fixed-width and short regardless of instance name
// length.
func macvtapIfaceName(instance string) string {
	sum := sha256.Sum256([]byte("expanse-vm-tap:" + instance))
	return "mv" + hex.EncodeToString(sum[:6]) // 2 + 12 = 14 chars, under the 15-byte cap
}

func createMacvtap(uplink, name, mac string) (*os.File, error) {
	if _, err := net.ParseMAC(mac); err != nil {
		return nil, fmt.Errorf("mac %q: %w", mac, err)
	}
	ip, err := resolveBin("ip")
	if err != nil {
		return nil, err
	}
	// Idempotent: a restart on the same node finds its own prior child
	// still present, the same "wipe and recreate before every start"
	// convention lioTarget.teardown/resetSMBEphemeralState both use.
	_ = exec.Command(ip, "link", "delete", name).Run()
	if err := ipLink(ip, "add", "link", uplink, "name", name, "type", "macvtap", "mode", "bridge"); err != nil {
		return nil, fmt.Errorf("create macvtap %s on %s: %w", name, uplink, err)
	}
	// address and up in one call: unlike netlink's own NEWLINK path,
	// `ip link set` combining both is exactly vm-macvtap-probe.py's own
	// measured sequence.
	if err := ipLink(ip, "set", name, "address", mac, "up"); err != nil {
		_ = exec.Command(ip, "link", "delete", name).Run()
		return nil, fmt.Errorf("set macvtap %s address/up: %w", name, err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		_ = exec.Command(ip, "link", "delete", name).Run()
		return nil, fmt.Errorf("look up macvtap %s after creation: %w", name, err)
	}
	// The kernel's macvtap driver exposes each child as a character
	// device whose minor number is the link's own ifindex — the
	// well-known /dev/tap<ifindex> convention libvirt's own macvtap
	// integration relies on.
	idx := link.Attrs().Index
	f, err := os.OpenFile(fmt.Sprintf("/dev/tap%d", idx), os.O_RDWR, 0)
	if err != nil {
		_ = exec.Command(ip, "link", "delete", name).Run()
		return nil, fmt.Errorf("open /dev/tap%d: %w", idx, err)
	}
	return f, nil
}

// ipLink runs one `ip link ...` command, folding its own error text
// into the returned error for diagnosability — the same shape
// targetcli() already uses for iscsi/target's one-shot setup commands.
func ipLink(ip string, args ...string) error {
	out, err := exec.Command(ip, append([]string{"link"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip link %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// deleteLink removes a link by name, if it exists. Errors are
// deliberately discarded, the same best-effort convention
// lioTarget.teardown uses: called both defensively before creating (a
// node that never ran this instance before has nothing to remove) and
// on shutdown.
func deleteLink(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil //nolint:nilerr // no such link: nothing to remove
	}
	return netlink.LinkDel(link)
}

// defaultMAC derives a stable, locally-administered guest MAC from the
// block instance's own name — mirrors defaultIQN/defaultWWN, the same
// "pin an identity once, never derive it from anything node-specific"
// pattern D2 uses for a VM's network identity, so ordinary switch
// MAC-learning and the guest's own gratuitous ARP on boot handle
// failover with no new cluster-level mechanism.
func defaultMAC(instance string) string {
	sum := sha256.Sum256([]byte("expanse-vm-mac:" + instance))
	b := make([]byte, 6)
	copy(b, sum[:6])
	// Locally-administered, unicast: clear the multicast bit, set the
	// locally-administered bit — the same OUI class QEMU/libvirt's own
	// default MAC scheme ("52:54:00:...") already uses.
	b[0] = (b[0] &^ 0x01) | 0x02
	return net.HardwareAddr(b).String()
}

// uplinkIfaceFromEnv resolves the EXPANSE_EXTERNAL_INTERFACE env var
// (nix/modules/agent.nix, set from the agent's own --external-interface)
// into the value vip.ResolveIface expects, defaulting to "auto" (the
// default route's device) when unset — the same "empty = auto-detect"
// convention the agent's own VIP holder already uses for the identical
// question.
func uplinkIfaceFromEnv(v string) string {
	if v == "" {
		return "auto"
	}
	return v
}

// vmArg returns the value of a bare "--name value" flag from args, or
// "" when absent — the same shape mountPaths/voldevs already parse,
// specialized for a single scalar.
func vmArg(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// vmCPUs resolves the guest's vCPU count from the --cpu arg
// (bridge.go's replicaSpec, sourced from spec.resources.requests.cpu) —
// D6's "guest-visible ceiling", not just the cgroup limit every other
// block type already gets for free. qemu's -smp wants a whole number;
// a fractional request rounds up, since a guest cannot usefully see
// half a vCPU.
func vmCPUs(args []string) int {
	raw := vmArg(args, "--cpu")
	if raw == "" {
		return 1
	}
	q, err := quantity.ParseCPU(raw)
	if err != nil {
		return 1
	}
	cores := int((q.Milli + 999) / 1000)
	if cores < 1 {
		return 1
	}
	return cores
}

// vmMemMiB resolves the guest's RAM ceiling in MiB from the --mem arg
// (D6, sourced from spec.resources.requests.memory). qemu's -m wants
// whole MiB; an unset or too-small request falls back to a floor small
// enough to still boot a minimal guest but large enough not to starve
// it outright (nix/tests/vm-d1-boot-probe.nix's own measured OOM-panic
// finding against an undersized guest).
func vmMemMiB(args []string) int {
	raw := vmArg(args, "--mem")
	if raw == "" {
		return 512
	}
	q, err := quantity.ParseBytes(raw)
	if err != nil {
		return 512
	}
	mib := int(q.N / (1 << 20))
	if mib < 128 {
		return 128
	}
	return mib
}

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
	sharedBuffers string, maxWalSenders, maxReplicationSlots int,
) error {
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
		// --data-checksums: off by default, but Stream C's X3 exit
		// criterion (amcheck + pg_checksums report zero corruption after
		// failover) is meaningless without it -- pg_checksums --check
		// simply refuses to run at all against a cluster where checksums
		// were never enabled. Copied verbatim into every standby by
		// pg_basebackup (a cluster-wide, physically replicated setting),
		// so only the primary's own initdb needs the flag.
		if err := pgCmd(ctx, nil, "", "initdb", "-D", pgdata, "--username=postgres", "--pwfile="+pwFile.Name(), "--data-checksums"); err != nil {
			return err
		}
		// sync=false: no standby can possibly be connected yet -- none
		// can even authenticate before the CREATE ROLE below commits --
		// so synchronous replication must not be active for this first,
		// temporary start (writePGConf's own doc comment has the full
		// deadlock this avoids).
		if err := writePGConf(pgdata, sockDir, port, sharedBuffers, maxWalSenders, maxReplicationSlots, false); err != nil {
			return err
		}
		// -l redirects the daemonized postmaster's own stdout/stderr to
		// a real file instead of leaving it to inherit this process's:
		// without it (X1 VM test), the postmaster pg_ctl forks keeps
		// those inherited pipe fds open for as long as it runs (i.e.
		// forever), so cmd.CombinedOutput's own wait for pipe EOF never
		// completes even though pg_ctl itself (and the postmaster) had
		// already started successfully — hanging this call forever and
		// never reaching the CREATE ROLE/CREATE DATABASE step below.
		bootLog := filepath.Join(filepath.Dir(sockDir), "bootstrap.log")
		if err := pgCmd(ctx, nil, "", "pg_ctl", "-D", pgdata, "-l", bootLog, "-w", "start"); err != nil {
			return err
		}
		sql := "CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD :'pass';\nCREATE DATABASE :\"dbname\";\n"
		createErr := pgCmd(ctx, nil, sql, "psql", "-h", sockDir, "-p", port, "-U", "postgres",
			"-v", "ON_ERROR_STOP=1", "-v", "pass="+replPassword, "-v", "dbname="+database)
		if stopErr := pgCmd(ctx, nil, "", "pg_ctl", "-D", pgdata, "-w", "stop"); stopErr != nil && createErr == nil {
			return stopErr
		}
		if createErr != nil {
			return createErr
		}
		// Now that the replicator role exists, the real, long-running
		// start (runPostgres's own execWorkload, after this function
		// returns) can safely require a synchronous standby -- one will
		// eventually connect and authenticate against it.
		return writePGConf(pgdata, sockDir, port, sharedBuffers, maxWalSenders, maxReplicationSlots, true)
	case "replica":
		env := []string{"PGPASSWORD=" + replPassword}
		// Create the slot as its own idempotent step rather than via
		// pg_basebackup's own -C: found via the X1 VM test, a transient
		// backup failure (once, a WAL segment recycled out from under a
		// slow copy under VM CPU contention) can leave the slot behind
		// after -C already created it, and every retry of a -C backup
		// then fails permanently with "slot already exists" -- pg_ctl
		// has no "if not exists" form. A slot that already exists here
		// is exactly the retry case, not an error.
		ensureSlot := "SELECT pg_create_physical_replication_slot(:'slot') WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = :'slot');\n"
		if err := pgCmd(ctx, env, ensureSlot, "psql", "-h", host, "-p", peerport, "-U", "replicator", "-d", "postgres",
			"-v", "ON_ERROR_STOP=1", "-v", "slot="+slot); err != nil {
			return err
		}
		// --no-verify-checksums: --data-checksums (above) is what X3
		// actually needs -- a later, EXPLICIT pg_checksums/amcheck pass,
		// not pg_basebackup's own opportunistic page-by-page
		// verification during the copy, which buys nothing X3 doesn't
		// already check independently while adding real CPU/IO cost to
		// every bootstrap and rejoin re-clone (X4) for no benefit here.
		//
		// Bounded, unlike every other step here: pg_basebackup is the one
		// call in this function that blocks on a full network data
		// transfer rather than a single fast statement, so it is the one
		// place an unbounded hang (a stuck TCP connection, e.g.) would
		// otherwise wedge this whole unit forever with no error to retry
		// from -- Restart=on-failure and the retry-next-pass idiom every
		// other step here relies on both need an actual failure to act
		// on, not silence.
		bbCtx, bbCancel := context.WithTimeout(ctx, pgBasebackupTimeout)
		err = pgCmd(bbCtx, env, "", "pg_basebackup",
			"-h", host, "-p", peerport, "-U", "replicator",
			"-D", pgdata, "-Fp", "-Xs", "-R", "-S", slot, "--no-verify-checksums")
		bbCancel()
		if err != nil {
			return err
		}
		// sync=true: a fresh standby authenticates as the replicator role
		// pg_basebackup itself already required to exist -- no deadlock,
		// unlike the primary's own first start above.
		return writePGConf(pgdata, sockDir, port, sharedBuffers, maxWalSenders, maxReplicationSlots, true)
	default:
		return fmt.Errorf("unrecognised role kind %q in %s", kind, roleFile)
	}
}
