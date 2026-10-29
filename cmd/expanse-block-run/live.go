package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/expanse/expanse/internal/blocks/livespec"
)

// reloader applies a replica's live spec changes on SIGHUP (livespec); the agent sends one only
// while the replica's marker says this run handles it.
type reloader struct {
	specDir, liveDir, instance, typ string
	apply                           func(cur, next []string) error
	restart                         func(instance string) error
}

// serveReloads serves SIGHUP reloads for the replica until ctx ends.
func serveReloads(ctx context.Context, instance, typ string, args []string, apply func(cur, next []string) error) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	r := reloader{specDir: specDir, liveDir: os.Getenv("LIVE_DIR"), instance: instance, typ: typ, apply: apply, restart: restartOwnUnit}
	r.serve(ctx, args, hup)
}

// serve applies each reload; one it can't apply restarts the unit, which starts on the new spec.
func (r reloader) serve(ctx context.Context, cur []string, hup <-chan os.Signal) {
	marker := livespec.MarkerPath(r.liveDir, r.instance)
	err := os.MkdirAll(filepath.Dir(marker), 0o755)
	if err == nil {
		err = os.WriteFile(marker, nil, 0o644)
	}
	if err != nil { // the agent then restarts this replica on any change
		fmt.Fprintf(os.Stderr, "expanse-block-run: %s: reload marker: %v\n", r.instance, err)
	}
	defer os.Remove(marker)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		}
		next, err := reloadFrom(r.specDir, r.instance, r.typ, cur, r.apply)
		if err == nil {
			cur = next
			continue
		}
		fmt.Printf("expanse-block-run: %s: %v; restarting it on its new spec\n", r.instance, err)
		_ = os.Remove(marker)
		if err := r.restart(r.instance); err != nil {
			fmt.Fprintf(os.Stderr, "expanse-block-run: %s: restart: %v\n", r.instance, err)
		}
		return
	}
}

// reloadFrom re-reads the replica's spec and applies it when only its type's live fields changed.
func reloadFrom(dir, instance, typ string, cur []string, apply func(cur, next []string) error) ([]string, error) {
	s, err := loadSpecAt(dir, instance)
	if err != nil {
		return nil, err
	}
	next, err := withCredentials(s.Args, os.Getenv("CREDENTIALS_DIRECTORY"))
	if err != nil {
		return nil, err
	}
	switch {
	case s.Stopped || s.Type != typ:
		return nil, errors.New("the new spec stops or replaces this replica")
	case slices.Equal(livespec.CanonicalArgs(cur), livespec.CanonicalArgs(next)):
		return cur, nil
	case !livespec.LiveOnly(typ, cur, next):
		return nil, errors.New("the spec changed beyond its live fields")
	}
	if err := apply(cur, next); err != nil {
		return nil, err
	}
	fmt.Printf("expanse-block-run: %s: applied its new spec live\n", instance)
	return next, nil
}

// restartOwnUnit queues a restart of the unit this process runs in; its stop runs as any other.
func restartOwnUnit(instance string) error {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	unit := unitFromCgroup(string(raw))
	if unit == "" {
		return fmt.Errorf("%s runs outside a service unit", instance)
	}
	path, err := resolveBin("systemctl")
	if err != nil {
		return err
	}
	return exec.Command(path, "--no-block", "restart", unit).Run()
}

// unitFromCgroup is the service unit in a /proc/self/cgroup (cgroup v2) line, or "".
func unitFromCgroup(cgroup string) string {
	unit := filepath.Base(strings.TrimSpace(cgroup))
	if !strings.HasSuffix(unit, ".service") {
		return ""
	}
	return unit
}
