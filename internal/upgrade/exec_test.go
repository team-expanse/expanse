package upgrade

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

func TestAFailedBuildReportsNixsErrorNotJustItsExitStatus(t *testing.T) {
	stderr := "copying path '/nix/store/a' from 'file:///c'...\n" +
		"error: builder for '/nix/store/x-etc.drv' failed with exit code 1;\n" +
		"       last 1 log lines:\n" +
		"       > No space left on device\n" +
		"copying path '/nix/store/b' from 'file:///c'...\n"
	got := buildFailure(stderr)
	if !strings.HasPrefix(got, "error: builder for") || !strings.Contains(got, "No space left on device") {
		t.Errorf("buildFailure = %q, want nix's error through to the end", got)
	}
	if strings.Contains(got, "/nix/store/a") {
		t.Errorf("buildFailure = %q, want the lines before the error dropped", got)
	}
}

func TestABuildFailureWithoutAnErrorLineKeepsItsLastLines(t *testing.T) {
	var b strings.Builder
	for i := range 30 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	got := buildFailure(b.String())
	if !strings.HasSuffix(got, "line 29") || strings.Contains(got, "line 10\n") {
		t.Errorf("buildFailure = %q, want only the last few lines", got)
	}
	if long := buildFailure("error: " + strings.Repeat("x", 5000)); len(long) > maxBuildFailure {
		t.Errorf("buildFailure kept %d bytes, want at most %d", len(long), maxBuildFailure)
	}
	if buildFailure("\n  \n") != "" {
		t.Error("empty stderr: want nothing to report")
	}
}
