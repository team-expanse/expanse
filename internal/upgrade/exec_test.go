package upgrade

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestBuildArgsTrustTheReleaseCacheOnlyWithItsKey(t *testing.T) {
	got := buildArgs("/etc/nixos/expanse/nix/node-system.nix", "file:///var/cache/c", "expanse-1:abc")
	want := []string{
		"/etc/nixos/expanse/nix/node-system.nix", "-A", "config.system.build.toplevel", "--no-out-link",
		"--option", "extra-substituters", "file:///var/cache/c",
		"--option", "extra-trusted-public-keys", "expanse-1:abc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if got := buildArgs("/f.nix", "", "expanse-1:abc"); len(got) != 4 {
		t.Errorf("without a cache: %v", got)
	}
}

func TestABuildKilledBySignalRunsOnceMore(t *testing.T) {
	runs := 0
	path, err := retryKilled(context.Background(), func() (string, string, error) {
		runs++
		if runs == 1 {
			return "", "       Reason: builder failed due to signal 9 (Killed).\n", errors.New("exit status 100")
		}
		return "/nix/store/x-system", "", nil
	})
	if err != nil || path != "/nix/store/x-system" || runs != 2 {
		t.Fatalf("got %q, %v after %d runs; want the second run's path", path, err, runs)
	}
}

func TestABuildKilledTwiceOrFailingOtherwiseStops(t *testing.T) {
	for name, stderr := range map[string]string{
		"killed twice": "builder failed due to signal 9 (Killed).",
		"build error":  "error: undefined variable 'foo'",
	} {
		runs := 0
		_, err := retryKilled(context.Background(), func() (string, string, error) {
			runs++
			return "", stderr, errors.New("exit status 1")
		})
		want := map[string]int{"killed twice": 2, "build error": 1}[name]
		if err == nil || runs != want {
			t.Errorf("%s: %v after %d runs, want an error after %d", name, err, runs, want)
		}
	}
}

func TestACancelledBuildIsNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runs := 0
	_, err := retryKilled(ctx, func() (string, string, error) {
		runs++
		cancel()
		return "", "builder failed due to signal 9 (Killed).", errors.New("signal: killed")
	})
	if err == nil || runs != 1 {
		t.Fatalf("%v after %d runs, want one failed run", err, runs)
	}
}
