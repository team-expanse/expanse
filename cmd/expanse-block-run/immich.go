package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// immichMLPort is where the machine-learning service listens, on loopback only.
const immichMLPort = "3003"

// immichSetup is everything one media/immich instance needs: state on the volume, sockets in private /tmp.
type immichSetup struct {
	PGData, RedisDir, Media, ModelCache string
	Scratch, Sockets, Port              string
	MachineLearning                     bool
}

// immichSetupFrom keeps socket paths short: Unix socket paths stop at 107 bytes.
func immichSetupFrom(mountPath, scratch string, cfg map[string]any) immichSetup {
	root := filepath.Join(mountPath, "immich")
	return immichSetup{
		PGData:          filepath.Join(root, "postgres"),
		RedisDir:        filepath.Join(root, "redis"),
		Media:           filepath.Join(root, "media"),
		ModelCache:      filepath.Join(root, "model-cache"),
		Scratch:         scratch,
		Sockets:         filepath.Join(scratch, "s"),
		Port:            cfgPortOr(cfg, "2283"),
		MachineLearning: cfgBool(cfg, "machineLearning", true),
	}
}

// immichPostgresArgs serves the private database on a Unix socket only; every commit is synced before it is acknowledged.
func immichPostgresArgs(s immichSetup) []string {
	return []string{
		"-D", s.PGData,
		"-k", s.Sockets,
		"-c", "listen_addresses=",
		"-c", "shared_preload_libraries=vchord.so",
		"-c", `search_path="$user", public, vectors`,
		"-c", "fsync=on",
		"-c", "synchronous_commit=on",
		"-c", "full_page_writes=on",
	}
}

// immichRedisConfig keeps Immich's job queue on the volume, each write synced before Redis replies.
func immichRedisConfig(s immichSetup) string {
	return fmt.Sprintf(`port 0
unixsocket %s
unixsocketperm 700
dir %s
appendonly yes
appendfsync always
save ""
`, filepath.Join(s.Sockets, "redis.sock"), s.RedisDir)
}

func immichServerEnv(s immichSetup) []string {
	return []string{
		"DB_URL=postgresql:///immich?host=" + s.Sockets + "&user=immich",
		"HOME=" + s.Scratch,
		"IMMICH_HOST=0.0.0.0",
		"IMMICH_MACHINE_LEARNING_URL=http://127.0.0.1:" + immichMLPort,
		"IMMICH_MEDIA_LOCATION=" + s.Media,
		"IMMICH_PORT=" + s.Port,
		"REDIS_SOCKET=" + filepath.Join(s.Sockets, "redis.sock"),
	}
}

func immichMachineLearningEnv(s immichSetup) []string {
	return []string{
		"HOME=" + s.Scratch,
		"IMMICH_HOST=127.0.0.1",
		"IMMICH_PORT=" + immichMLPort,
		"MACHINE_LEARNING_CACHE_FOLDER=" + s.ModelCache,
		"MACHINE_LEARNING_WORKERS=1",
		"MACHINE_LEARNING_WORKER_TIMEOUT=120",
		"MPLCONFIGDIR=" + filepath.Join(s.Scratch, "matplotlib"),
		"XDG_CACHE_HOME=" + filepath.Join(s.Scratch, "cache"),
	}
}

// checkVectorChord fails unless the postgres at bin was built with the vectorchord extension.
func checkVectorChord(bin string) error {
	lib := filepath.Join(filepath.Dir(filepath.Dir(bin)), "lib", "vchord.so")
	if _, err := os.Stat(lib); err != nil {
		return fmt.Errorf("media/immich: %s lacks the vectorchord extension; ship "+
			"postgresql_18.withPackages (ps: [ ps.pgvector ps.vectorchord ]): %w", bin, err)
	}
	return nil
}

// ensureImmichPostgres initialises pgdata once, in a side directory renamed into place,
// so a crash mid-initdb never leaves a half cluster.
func ensureImmichPostgres(pgdata string, initdb func(dir string) error) error {
	if _, err := os.Stat(filepath.Join(pgdata, "PG_VERSION")); err == nil {
		return nil
	}
	tmp := pgdata + ".init"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := initdb(tmp); err != nil {
		return err
	}
	if err := os.RemoveAll(pgdata); err != nil {
		return err
	}
	if err := os.Rename(tmp, pgdata); err != nil {
		return err
	}
	return syncFS(filepath.Dir(pgdata))
}

// immichInitdb creates a UTF-8 cluster owned by the immich role, with the immich database in it.
func immichInitdb(ctx context.Context, postgres string) func(dir string) error {
	return func(dir string) error {
		if err := pgCmd(ctx, nil, "", "initdb", "-D", dir, "--username=immich", "--auth=trust",
			"--encoding=UTF8", "--no-locale"); err != nil {
			return err
		}
		return pgCmd(ctx, nil, "CREATE DATABASE immich;\n", postgres, "--single", "-D", dir, "postgres")
	}
}

// immichProc is one supervised child, stopped with its own shutdown signal; err is set before exited closes.
type immichProc struct {
	name   string
	cmd    *exec.Cmd
	stop   syscall.Signal
	exited chan struct{}
	err    error
}

func startImmichProc(name, bin string, args, env []string, stop syscall.Signal) (*immichProc, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Its own process group, so halt reaches the workers Immich's server forks.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("media/immich: starting %s: %w", name, err)
	}
	p := &immichProc{name: name, cmd: cmd, stop: stop, exited: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// exitErr is why p exited; a clean exit is still an error for a service meant to keep running.
func (p *immichProc) exitErr() error {
	if p.err == nil {
		return errors.New("exit status 0")
	}
	return p.err
}

// halt signals p's process group and kills the group if p outlives timeout; leftovers in it are killed either way.
func (p *immichProc) halt(timeout time.Duration) {
	pgid := -p.cmd.Process.Pid
	_ = syscall.Kill(pgid, p.stop)
	select {
	case <-p.exited:
	case <-time.After(timeout):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-p.exited
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
}

// superviseImmich waits for a deliberate stop or any child's exit, then stops the
// rest in reverse start order so the server goes before the database it writes to.
func superviseImmich(ctx context.Context, procs []*immichProc, timeout time.Duration) error {
	first := make(chan *immichProc, len(procs))
	for _, p := range procs {
		go func() {
			<-p.exited
			first <- p
		}()
	}
	var err error
	select {
	case <-ctx.Done():
	case p := <-first:
		err = fmt.Errorf("media/immich: %s exited: %w", p.name, p.exitErr())
	}
	for i := len(procs) - 1; i >= 0; i-- {
		procs[i].halt(timeout)
	}
	if ctx.Err() != nil {
		return nil //nolint:nilerr // deliberate stop
	}
	return err
}

// waitImmichReady polls ready until it succeeds, the child dies or ctx ends.
func waitImmichReady(ctx context.Context, p *immichProc, ready func() bool) error {
	for !ready() {
		select {
		case <-p.exited:
			return fmt.Errorf("media/immich: %s exited before it was ready: %w", p.name, p.exitErr())
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil
}

// runImmich serves media/immich from the volume; the SINGLETON strategy keeps one writer.
func runImmich(ctx context.Context, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	mountPath := firstMount(mountPaths(args))
	if mountPath == "" {
		return errors.New("media/immich: no bound storage mount yet")
	}
	if err := waitForMount(mountPath, 30*time.Second, 500*time.Millisecond, realStat); err != nil {
		return fmt.Errorf("media/immich: %w", err)
	}
	s := immichSetupFrom(mountPath, filepath.Join(os.TempDir(), "immich"), cfg)
	bins, err := immichBins(s.MachineLearning)
	if err != nil {
		return err
	}
	for _, dir := range []string{s.RedisDir, s.Media, s.ModelCache, s.Sockets} {
		if err := mkdirAllRetrying(os.MkdirAll, dir, 0o700, 10, 500*time.Millisecond); err != nil {
			return fmt.Errorf("media/immich: %w", err)
		}
	}
	if err := ensureImmichPostgres(s.PGData, immichInitdb(ctx, bins["postgres"])); err != nil {
		return fmt.Errorf("media/immich: initialising %s: %w", s.PGData, err)
	}
	redisConf := filepath.Join(s.Scratch, "redis.conf")
	if err := os.WriteFile(redisConf, []byte(immichRedisConfig(s)), 0o600); err != nil {
		return err
	}
	// Uploads are flushed by Immich itself; thumbnails and the rest are written without fsync.
	go syncLoop(ctx, mountPath, 2*time.Second, unix.Syncfs)
	procs, err := startImmich(ctx, s, bins, redisConf)
	if err != nil {
		for i := len(procs) - 1; i >= 0; i-- {
			procs[i].halt(time.Minute)
		}
		if ctx.Err() != nil {
			return nil //nolint:nilerr // deliberate stop
		}
		return err
	}
	fmt.Printf("expanse-block-run: immich serving on :%s from %s\n", s.Port, s.Media)
	return superviseImmich(ctx, procs, 2*time.Minute)
}

// immichBins finds every program the block runs, failing early on one that is not shipped.
func immichBins(machineLearning bool) (map[string]string, error) {
	names := []string{"postgres", "redis-server", "immich-admin"}
	if machineLearning {
		names = append(names, "machine-learning")
	}
	bins := map[string]string{}
	for _, name := range names {
		bin, err := resolveBin(name)
		if err != nil {
			return nil, fmt.Errorf("block runtime: %s not found in PATH (ship the package): %w", name, err)
		}
		bins[name] = bin
	}
	if err := checkVectorChord(bins["postgres"]); err != nil {
		return nil, err
	}
	// The server's own program is a sibling of immich-admin, under a generic name.
	bins["server"] = filepath.Join(filepath.Dir(bins["immich-admin"]), "server")
	return bins, nil
}

// startImmich starts the database and queue, waits until they answer, then the services that use them.
func startImmich(ctx context.Context, s immichSetup, bins map[string]string, redisConf string) ([]*immichProc, error) {
	var procs []*immichProc
	pg, err := startImmichProc("postgres", bins["postgres"], immichPostgresArgs(s), nil, syscall.SIGINT)
	if err != nil {
		return procs, err
	}
	procs = append(procs, pg)
	if err := waitImmichReady(ctx, pg, func() bool {
		return pgCmd(ctx, nil, "", "pg_isready", "-q", "-h", s.Sockets, "-U", "immich", "-d", "immich") == nil
	}); err != nil {
		return procs, err
	}
	redis, err := startImmichProc("redis", bins["redis-server"], []string{redisConf}, nil, syscall.SIGTERM)
	if err != nil {
		return procs, err
	}
	procs = append(procs, redis)
	if err := waitImmichReady(ctx, redis, func() bool {
		return unixSocketAnswers(filepath.Join(s.Sockets, "redis.sock"))
	}); err != nil {
		return procs, err
	}
	if s.MachineLearning {
		ml, err := startImmichProc("machine-learning", bins["machine-learning"], nil, immichMachineLearningEnv(s), syscall.SIGTERM)
		if err != nil {
			return procs, err
		}
		procs = append(procs, ml)
	}
	server, err := startImmichProc("server", bins["server"], nil, immichServerEnv(s), syscall.SIGTERM)
	if err != nil {
		return procs, err
	}
	return append(procs, server), nil
}

// unixSocketAnswers reports whether something accepts connections on path.
func unixSocketAnswers(path string) bool {
	c, err := net.Dial("unix", path)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
