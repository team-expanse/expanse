package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// State is where a node's upgrade stands.
type State string

const (
	StateFetching   State = "fetching"
	StateInstalling State = "installing"
	StateBuilding   State = "building"
	StateSwitching  State = "switching"
	StateDone       State = "done"
	StateFailed     State = "failed"
)

// Status is the node's last upgrade, kept in a file the agent reports to the cluster.
type Status struct {
	Version string `json:"version"`
	From    string `json:"from,omitempty"`
	State   State  `json:"state"`
	Error   string `json:"error,omitempty"`
	// System is the built system closure, once there is one.
	System string `json:"system,omitempty"`
	// RebootNeeded: the new system's kernel or initrd differs from the booted one.
	RebootNeeded bool      `json:"rebootNeeded,omitempty"`
	Started      time.Time `json:"started"`
	Updated      time.Time `json:"updated"`
}

// Nix builds and activates a node system.
type Nix interface {
	// BuildSystem builds node-system.nix's toplevel; substituter is "" or an extra cache URL.
	BuildSystem(ctx context.Context, file, substituter string, logs io.Writer) (string, error)
	SetProfile(ctx context.Context, path string) error
	// Switch activates path, arming the switch watchdog for the duration.
	Switch(ctx context.Context, path string) error
}

// Upgrader runs one node's upgrade.
type Upgrader struct {
	Source     *Source
	Tree       *Tree
	Nix        Nix
	StatusPath string
	// BootedSystem is /run/booted-system, compared with the new system for RebootNeeded.
	BootedSystem string
}

// Apply moves this node to version. A failed build puts the previous source back.
func (u *Upgrader) Apply(ctx context.Context, version string, logs io.Writer) error {
	from, _ := u.Tree.Version(currentDir)
	st := &Status{Version: version, From: from, Started: time.Now()}
	if err := u.run(ctx, st, logs); err != nil {
		st.State, st.Error = StateFailed, err.Error()
		_ = u.save(st)
		return err
	}
	st.State = StateDone
	return u.save(st)
}

func (u *Upgrader) run(ctx context.Context, st *Status, logs io.Writer) error {
	if err := u.step(st, StateFetching); err != nil {
		return err
	}
	rel, err := u.Source.Fetch(ctx, st.Version)
	if err != nil {
		return err
	}
	if err := u.step(st, StateInstalling); err != nil {
		return err
	}
	substituter, err := unpackCache(rel)
	if err != nil {
		return err
	}
	if err := u.Tree.Install(rel.Tarball, st.Version); err != nil {
		return err
	}
	if err := u.step(st, StateBuilding); err != nil {
		return err
	}
	file := filepath.Join(u.Tree.Dir, currentDir, "nix", "node-system.nix")
	st.System, err = u.Nix.BuildSystem(ctx, file, substituter, logs)
	if err != nil {
		if rerr := u.Tree.Restore(); rerr != nil {
			return fmt.Errorf("build: %w (and restoring the previous source: %w)", err, rerr)
		}
		return fmt.Errorf("build: %w", err)
	}
	if err := u.step(st, StateSwitching); err != nil {
		return err
	}
	if err := u.Nix.SetProfile(ctx, st.System); err != nil {
		return fmt.Errorf("set the system profile: %w", err)
	}
	st.RebootNeeded = u.rebootNeeded(st.System)
	if err := u.Nix.Switch(ctx, st.System); err != nil {
		return fmt.Errorf("switch: %w", err)
	}
	return nil
}

func (u *Upgrader) step(st *Status, s State) error {
	st.State = s
	return u.save(st)
}

func (u *Upgrader) save(st *Status) error {
	st.Updated = time.Now()
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.StatusPath), 0o750); err != nil {
		return err
	}
	tmp := u.StatusPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, u.StatusPath)
}

// rebootNeeded compares the kernel, initrd and modules the node booted with the new system's.
func (u *Upgrader) rebootNeeded(system string) bool {
	booted := u.BootedSystem
	if booted == "" {
		return false
	}
	for _, name := range []string{"kernel", "initrd", "kernel-modules"} {
		a, errA := filepath.EvalSymlinks(filepath.Join(booted, name))
		b, errB := filepath.EvalSymlinks(filepath.Join(system, name))
		if errA != nil || errB != nil || a != b {
			return true
		}
	}
	return false
}

// unpackCache unpacks the release's binary cache beside its download, as a file:// substituter.
func unpackCache(rel *Fetched) (string, error) {
	if rel.Cache == "" {
		return "", nil
	}
	dir := filepath.Join(filepath.Dir(rel.Cache), "cache")
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := unpack(rel.Cache, "", dir); err != nil {
		return "", fmt.Errorf("unpack the release cache: %w", err)
	}
	return "file://" + dir, nil
}

// ReadStatus reads a node's last upgrade status.
func ReadStatus(path string) (*Status, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st Status
	return &st, json.Unmarshal(b, &st)
}
