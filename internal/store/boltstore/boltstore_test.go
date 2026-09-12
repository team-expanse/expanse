package boltstore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/conformance"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestConformance runs the full shared conformance suite (reused by
// raftstore in Phase 03).
func TestConformance(t *testing.T) {
	conformance.RunConformance(t, newTestStore)
}

// TestPersistenceAcrossRestart verifies G2.2: data survives Close/reopen.
func TestPersistenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := s.Put(context.Background(), "/node/x/resources/file:/etc/motd", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	e, err := s2.Get(context.Background(), "/node/x/resources/file:/etc/motd")
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if string(e.Value) != "hello" || e.Revision != r1 {
		t.Errorf("after restart: value=%q rev=%d, want %q %d", e.Value, e.Revision, "hello", r1)
	}
	// Revision counter must continue monotonically after restart.
	r2, err := s2.Put(context.Background(), "/other", []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	if r2 <= r1 {
		t.Errorf("revision after restart %d <= before %d (counter regressed)", r2, r1)
	}
}

// TestKill9MidWrite verifies G2.13: a SIGKILL mid-write never corrupts the
// store, and every acknowledged write is present afterwards. The child
// process (re-exec of this test binary) writes keys in a loop until killed.
func TestKill9MidWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test skipped in -short mode")
	}
	if os.Getenv("EXpanse_KILL9_CHILD") == "1" {
		kill9Child()
		return
	}

	path := filepath.Join(t.TempDir(), "chaos.db")
	// Reader counter shared with the child via a file: the child appends
	// each key name after the Put is acknowledged; the parent then asserts
	// every acknowledged key is present in the reopened store.
	ackPath := filepath.Join(t.TempDir(), "acked.txt")

	const iterations = 20
	for i := 0; i < iterations; i++ {
		cmd := execTestBinary(t, "-test.run=TestKill9MidWrite", map[string]string{
			"EXpanse_KILL9_CHILD": "1",
			"EXpanse_STORE_PATH":  path,
			"EXpanse_ACK_PATH":    ackPath,
		})
		start := time.Now()
		go func() {
			// Kill at a random-ish point mid-write.
			d := time.Duration(10+i*3) * time.Millisecond
			time.Sleep(d)
			cmd.Process.Signal(syscall.SIGKILL)
		}()
		_ = cmd.Wait()
		if time.Since(start) < 5*time.Millisecond {
			t.Fatalf("child exited too fast at iteration %d", i)
		}

		s, err := New(path)
		if err != nil {
			t.Fatalf("iteration %d: store failed to open after SIGKILL: %v", i, err)
		}
		acked, err := os.ReadFile(ackPath)
		if err != nil {
			t.Fatal(err)
		}
		var lastRev store.Revision
		for _, key := range splitLines(acked) {
			e, err := s.Get(context.Background(), store.Key(key))
			if err != nil {
				t.Fatalf("iteration %d: acknowledged key %s missing after crash: %v", i, key, err)
			}
			if e.Revision <= lastRev {
				t.Fatalf("iteration %d: revision regressed (%d after %d)", i, e.Revision, lastRev)
			}
			lastRev = e.Revision
		}
		s.Close()
		os.Remove(ackPath)
	}
}

func kill9Child() {
	s, err := New(os.Getenv("EXpanse_STORE_PATH"))
	if err != nil {
		os.Exit(2)
	}
	defer s.Close()
	ack, err := os.OpenFile(os.Getenv("EXpanse_ACK_PATH"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		os.Exit(3)
	}
	defer ack.Close()
	ctx := context.Background()
	var i int
	for {
		i++
		key := store.Key(fmt.Sprintf("/chaos/%d", i))
		_, err := s.Put(ctx, key, []byte("v"))
		if err != nil {
			os.Exit(4)
		}
		if _, err := ack.WriteString(string(key) + "\n"); err != nil {
			os.Exit(5)
		}
	}
}

func execTestBinary(t *testing.T, runArg string, env map[string]string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], runArg, "-test.timeout=30s")
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	return cmd
}

func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

// TestWatchSurvivesConcurrentRegistration guards against the registry
// locking bugs the original draft had (RLock under Lock; self-comparison
// on unregister).
func TestWatchSurvivesConcurrentRegistration(t *testing.T) {
	s := newTestStore(t).(*Store)
	ctx := context.Background()
	var wg sync.WaitGroup
	var okCount atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wctx, cancel := context.WithCancel(ctx)
			ch, err := s.Watch(wctx, "/", 0)
			if err != nil {
				cancel()
				return
			}
			go func() {
				for range ch {
					okCount.Add(1)
				}
			}()
			time.Sleep(5 * time.Millisecond)
			cancel()
		}()
	}
	for i := 0; i < 200; i++ {
		if _, err := s.Put(ctx, store.Key(fmt.Sprintf("/k%d", i)), []byte("v")); err != nil {
			t.Fatalf("Put under concurrent watch churn: %v", err)
		}
	}
	wg.Wait()
	if okCount.Load() == 0 {
		t.Error("no watch events delivered under concurrent registration churn")
	}
}
