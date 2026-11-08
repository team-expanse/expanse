package upgrade

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func freeBytes(n uint64) FreeFunc {
	return func(string) (uint64, error) { return n, nil }
}

func withCache(t *testing.T, r *applyRig) {
	t.Helper()
	cache := writeTarball(t, map[string]string{"nix-cache-info": "StoreDir: /nix/store\n"})
	rel := newTestRelease(t, "1.12.0", map[string][]byte{
		"expanse-1.12.0.tar.gz": mustRead(t, writeTarball(t, release("1.12.0"))),
		CacheName("1.12.0"):     mustRead(t, cache),
	})
	r.u.Source = rel.source(t)
}

func TestFetchRefusesACacheThereIsNoRoomFor(t *testing.T) {
	r := newTestRelease(t, "1.12.0", map[string][]byte{
		"expanse-1.12.0.tar.gz": []byte("tarball"),
		CacheName("1.12.0"):     []byte("cache"),
	})
	src := r.source(t)
	src.Free = freeBytes(BuildReserve)
	_, err := src.Fetch(context.Background(), "1.12.0")
	if err == nil || !strings.Contains(err.Error(), "not enough disk space") || !strings.Contains(err.Error(), CacheName("1.12.0")) {
		t.Fatalf("err = %v, want one naming the cache and the space it needs", err)
	}
	if _, err := os.Stat(filepath.Join(src.CacheDir, "1.12.0", CacheName("1.12.0"))); !os.IsNotExist(err) {
		t.Errorf("the cache was downloaded anyway: %v", err)
	}
}

func TestFetchDownloadsACacheThatFits(t *testing.T) {
	r := newTestRelease(t, "1.12.0", map[string][]byte{
		"expanse-1.12.0.tar.gz": []byte("tarball"),
		CacheName("1.12.0"):     []byte("cache"),
	})
	src := r.source(t)
	src.Free = freeBytes(BuildReserve + 10)
	if _, err := src.Fetch(context.Background(), "1.12.0"); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRefusesToBuildWithoutRoomInTheNixStore(t *testing.T) {
	r := newApplyRig(t)
	r.u.NixStore = "/nix/store"
	r.u.Source.Free = func(path string) (uint64, error) {
		if path == "/nix/store" {
			return BuildReserve - 1, nil
		}
		return 1 << 40, nil
	}
	err := r.u.Apply(context.Background(), "1.12.0", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "/nix/store") {
		t.Fatalf("err = %v, want one naming /nix/store", err)
	}
	if len(r.nix.calls) != 0 {
		t.Errorf("built anyway: %v", r.nix.calls)
	}
	if v, _ := r.u.Tree.Version("expanse"); v != "1.11.3" {
		t.Errorf("source is %q, want the old one untouched", v)
	}
}

func TestASpaceCheckThatCannotRunDoesNotBlock(t *testing.T) {
	err := needSpace(func(string) (uint64, error) { return 0, errors.New("statfs: no such file") }, "/x", "y", 1)
	if err != nil {
		t.Fatalf("err = %v, want the upgrade left to find out for itself", err)
	}
}

// The unpacked cache is only for the build; the verified tarball stays for a retry.
func TestApplyRemovesTheUnpackedCacheAfterTheBuild(t *testing.T) {
	for _, fail := range []bool{true, false} {
		r := newApplyRig(t)
		withCache(t, r)
		if fail {
			r.nix.buildErr = errors.New("builder failed")
		}
		_ = r.u.Apply(context.Background(), "1.12.0", io.Discard)
		dir := filepath.Join(r.u.Source.CacheDir, "1.12.0")
		if _, err := os.Stat(filepath.Join(dir, "cache")); !os.IsNotExist(err) {
			t.Errorf("failed build %v: the unpacked cache survived: %v", fail, err)
		}
		if _, err := os.Stat(filepath.Join(dir, CacheName("1.12.0"))); fail && err != nil {
			t.Errorf("a failed build dropped the verified cache download: %v", err)
		}
	}
}
