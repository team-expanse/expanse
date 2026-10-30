package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestABootstrapStoppedHalfwayLeavesNoPgdataBehind(t *testing.T) {
	pgdata := filepath.Join(t.TempDir(), "pgdata")
	err := bootstrapInto(pgdata, func(dir string) error {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		// pg_basebackup copied the primary's files, then the unit stopped before writePGConf.
		_ = os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600)
		return errors.New("stopped")
	})
	if err == nil {
		t.Fatal("want the build's error")
	}
	if _, err := os.Stat(filepath.Join(pgdata, "PG_VERSION")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a half-made pgdata is in place: %v", err)
	}
}

func TestACompleteBootstrapReplacesAPartialPgdata(t *testing.T) {
	pgdata := filepath.Join(t.TempDir(), "pgdata")
	if err := os.MkdirAll(filepath.Join(pgdata, "base"), 0o700); err != nil {
		t.Fatal(err)
	}
	staging := pgdata + ".new"
	if err := os.MkdirAll(staging, 0o700); err != nil { // left by an earlier, stopped bootstrap
		t.Fatal(err)
	}
	err := bootstrapInto(pgdata, func(dir string) error {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the build's dir should start absent: %v", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pgdata, "PG_VERSION")); err != nil {
		t.Fatalf("pgdata not in place: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pgdata, "base")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the partial pgdata's files survived: %v", err)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging dir left behind: %v", err)
	}
}
