package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForPrimaryLeaseIgnoresAHeldMarkFromBeforeTheStart(t *testing.T) {
	dir := t.TempDir()
	held := filepath.Join(dir, "held")
	if err := os.WriteFile(held, []byte("held\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := waitForPrimaryLease(context.Background(), dir, 30*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("a held mark left from before the start let postgres start as primary")
	}
}

func TestWaitForPrimaryLeaseReturnsOnceTheLeaseIsMarkedHeld(t *testing.T) {
	dir := t.TempDir()
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(dir, "held"), []byte("held\n"), 0o644)
	}()
	if err := waitForPrimaryLease(context.Background(), dir, time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("waitForPrimaryLease: %v", err)
	}
}

func TestWaitForPrimaryLeaseReportsADemotion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "role"), []byte("replica 10.42.0.9 5432\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := waitForPrimaryLease(context.Background(), dir, time.Second, time.Millisecond)
	if !errors.Is(err, errPGDemoted) {
		t.Fatalf("err = %v, want errPGDemoted", err)
	}
}

func TestAwaitPrimaryLeaseWipesADemotedPrimarysData(t *testing.T) {
	mount := t.TempDir()
	stateDir, pgdata := filepath.Join(mount, ".expanse-postgres"), filepath.Join(mount, "pgdata")
	for _, d := range []string{stateDir, pgdata} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stateDir, "role"), []byte("replica 10.42.0.9 5432\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := awaitPrimaryLease(context.Background(), stateDir, pgdata, time.Second, time.Millisecond); err == nil {
		t.Fatal("a demoted primary must exit non-zero so the unit re-clones")
	}
	if _, err := os.Stat(pgdata); !os.IsNotExist(err) {
		t.Fatalf("a demoted primary kept its diverged pgdata (stat err %v)", err)
	}
}
