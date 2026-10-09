package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestImmichSetupKeepsStateOnTheVolume(t *testing.T) {
	s := immichSetupFrom("/vol", "/tmp/immich", map[string]any{})
	want := immichSetup{
		PGData:          "/vol/immich/postgres",
		RedisDir:        "/vol/immich/redis",
		Media:           "/vol/immich/media",
		ModelCache:      "/vol/immich/model-cache",
		Scratch:         "/tmp/immich",
		Sockets:         "/tmp/immich/s",
		Port:            "2283",
		MachineLearning: true,
	}
	if s != want {
		t.Errorf("setup = %+v, want %+v", s, want)
	}
	s = immichSetupFrom("/vol", "/tmp/immich", map[string]any{"port": float64(2300), "machineLearning": false})
	if s.Port != "2300" || s.MachineLearning {
		t.Errorf("port = %s, machineLearning = %v", s.Port, s.MachineLearning)
	}
}

func TestImmichPostgresArgsForceDurableCommits(t *testing.T) {
	got := immichPostgresArgs(immichSetupFrom("/vol", "/tmp/immich", map[string]any{}))
	want := []string{
		"-D", "/vol/immich/postgres",
		"-k", "/tmp/immich/s",
		"-c", "listen_addresses=",
		"-c", "shared_preload_libraries=vchord.so",
		"-c", `search_path="$user", public, vectors`,
		"-c", "fsync=on",
		"-c", "synchronous_commit=on",
		"-c", "full_page_writes=on",
	}
	if !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestImmichRedisConfigSyncsEveryJob(t *testing.T) {
	conf := immichRedisConfig(immichSetupFrom("/vol", "/tmp/immich", map[string]any{}))
	for _, want := range []string{
		"port 0\n",
		"unixsocket /tmp/immich/s/redis.sock\n",
		"dir /vol/immich/redis\n",
		"appendonly yes\n",
		"appendfsync always\n",
		"save \"\"\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("redis.conf lacks %q:\n%s", want, conf)
		}
	}
}

func TestImmichServerEnv(t *testing.T) {
	got := immichServerEnv(immichSetupFrom("/vol", "/tmp/immich", map[string]any{}))
	want := []string{
		"DB_URL=postgresql:///immich?host=/tmp/immich/s&user=immich",
		"HOME=/tmp/immich",
		"IMMICH_HOST=0.0.0.0",
		"IMMICH_MACHINE_LEARNING_URL=http://127.0.0.1:3003",
		"IMMICH_MEDIA_LOCATION=/vol/immich/media",
		"IMMICH_PORT=2283",
		"REDIS_SOCKET=/tmp/immich/s/redis.sock",
	}
	if !slices.Equal(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
}

func TestImmichMachineLearningEnv(t *testing.T) {
	got := immichMachineLearningEnv(immichSetupFrom("/vol", "/tmp/immich", map[string]any{}))
	want := []string{
		"HOME=/tmp/immich",
		"IMMICH_HOST=127.0.0.1",
		"IMMICH_PORT=3003",
		"MACHINE_LEARNING_CACHE_FOLDER=/vol/immich/model-cache",
		"MACHINE_LEARNING_WORKERS=1",
		"MACHINE_LEARNING_WORKER_TIMEOUT=120",
		"MPLCONFIGDIR=/tmp/immich/matplotlib",
		"XDG_CACHE_HOME=/tmp/immich/cache",
	}
	if !slices.Equal(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
}

func TestImmichPostgresNeedsVectorChord(t *testing.T) {
	prefix := t.TempDir()
	bin := filepath.Join(prefix, "bin", "postgres")
	if err := checkVectorChord(bin); err == nil || !strings.Contains(err.Error(), "vectorchord") {
		t.Errorf("err = %v, want one naming vectorchord", err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "lib", "vchord.so"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkVectorChord(bin); err != nil {
		t.Errorf("err = %v with vchord.so present", err)
	}
}

func TestEnsureImmichPostgresInitialisesOnce(t *testing.T) {
	pgdata := filepath.Join(t.TempDir(), "postgres")
	calls := 0
	initdb := func(dir string) error {
		calls++
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("18\n"), 0o600)
	}
	for range 2 {
		if err := ensureImmichPostgres(pgdata, initdb); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("initdb ran %d times, want 1", calls)
	}
	if _, err := os.Stat(pgdata + ".init"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("side directory left behind: %v", err)
	}
}

func TestEnsureImmichPostgresDiscardsAHalfInit(t *testing.T) {
	pgdata := filepath.Join(t.TempDir(), "postgres")
	if err := os.MkdirAll(pgdata+".init", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pgdata+".init", "stale"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	initdb := func(dir string) error {
		if _, err := os.Stat(filepath.Join(dir, "stale")); err == nil {
			return errors.New("initdb ran over a half-initialised directory")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("18\n"), 0o600)
	}
	if err := ensureImmichPostgres(pgdata, initdb); err != nil {
		t.Fatal(err)
	}
}

// shProc starts a shell running script, stopped with stop.
func shProc(t *testing.T, name, script string, stop syscall.Signal) *immichProc {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	p, err := startImmichProc(name, sh, []string{"-c", script}, nil, stop)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSuperviseImmichStopsTheRestInReverseOrder(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	trap := func(name string) string {
		return "trap 'echo " + name + " >> " + log + "; exit 0' TERM INT; while :; do sleep 0.05; done"
	}
	procs := []*immichProc{
		shProc(t, "postgres", trap("postgres"), syscall.SIGINT),
		shProc(t, "redis", trap("redis"), syscall.SIGTERM),
		shProc(t, "server", "sleep 0.3; exit 3", syscall.SIGTERM),
	}
	err := superviseImmich(t.Context(), procs, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "server") {
		t.Errorf("err = %v, want one naming server", err)
	}
	got, _ := os.ReadFile(log)
	if string(got) != "redis\npostgres\n" {
		t.Errorf("stop order = %q, want redis then postgres", got)
	}
}

func TestSuperviseImmichDeliberateStopIsNotAnError(t *testing.T) {
	p := shProc(t, "redis", "trap 'exit 0' TERM; while :; do sleep 0.05; done", syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := superviseImmich(ctx, []*immichProc{p}, 5*time.Second); err != nil {
		t.Errorf("err = %v, want nil on a deliberate stop", err)
	}
}

func TestImmichProcHaltStopsItsChildrenToo(t *testing.T) {
	// Immich's server forks API and microservices workers that outlive a signal to their parent alone.
	pidFile := filepath.Join(t.TempDir(), "child")
	p := shProc(t, "server", "sleep 30 & echo $! > "+pidFile+"; wait", syscall.SIGTERM)
	var raw []byte
	for range 100 {
		if raw, _ = os.ReadFile(pidFile); len(raw) > 0 && raw[len(raw)-1] == '\n' {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var child int
	if _, err := fmt.Sscan(string(raw), &child); err != nil {
		t.Fatalf("child pid %q: %v", raw, err)
	}
	p.halt(5 * time.Second)
	for range 100 {
		if syscall.Kill(child, 0) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("child %d outlived its halted parent", child)
	_ = syscall.Kill(child, syscall.SIGKILL)
}
