package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/reconcile"
	"github.com/expanse/expanse/internal/store"
)

// unitManager stands in for the block units a node runs, and counts what is torn down.
type unitManager struct{ deleted atomic.Int32 }

type unit struct{ id string }

func (u unit) ID() string             { return u.id }
func (u unit) Type() string           { return "unit" }
func (u unit) Dependencies() []string { return nil }

func (m *unitManager) Type() string                                         { return "unit" }
func (m *unitManager) Load(id string, _ []byte) (reconcile.Resource, error) { return unit{id}, nil }
func (m *unitManager) Observe(context.Context, reconcile.Resource) (reconcile.Observed, error) {
	return reconcile.Observed{Exists: true}, nil
}
func (m *unitManager) Plan(context.Context, reconcile.Resource, reconcile.Observed) ([]reconcile.Action, error) {
	return nil, nil
}
func (m *unitManager) Apply(context.Context, reconcile.Action) error { return nil }
func (m *unitManager) Delete(context.Context, reconcile.Resource) error {
	m.deleted.Add(1)
	return nil
}

func newRemovableAgent(t *testing.T, dir string) (*Agent, *unitManager) {
	t.Helper()
	a, err := New(Config{NodeID: "n1", DataDir: dir, Socket: filepath.Join(dir, "a.sock")})
	if err != nil {
		t.Fatal(err)
	}
	m := &unitManager{}
	a.recon.Register(m)
	return a, m
}

func revoke(t *testing.T, st store.Store, id string) {
	t.Helper()
	raw, _ := json.Marshal(nodelc.Revocation{NodeID: id, By: "n2", Reason: "decommissioned"})
	if _, err := st.Put(context.Background(), store.Key(nodelc.RevokedKeyPrefix+id), raw); err != nil {
		t.Fatal(err)
	}
}

func runUntilStopped(t *testing.T, a *Agent) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("the agent kept running")
		return nil
	}
}

func TestARemovedNodeTearsDownItsWorkloadsAndStops(t *testing.T) {
	defer func(d time.Duration) { removalPoll = d }(removalPoll)
	removalPoll = 50 * time.Millisecond
	dir := t.TempDir()
	a, m := newRemovableAgent(t, dir)
	if _, err := a.store.Put(context.Background(), a.recon.DesiredPrefix()+"web-0", []byte("type: unit\n")); err != nil {
		t.Fatal(err)
	}
	revoke(t, a.store, "n1")
	if err := runUntilStopped(t, a); !errors.Is(err, ErrRemoved) {
		t.Fatalf("Run returned %v, want ErrRemoved", err)
	}
	if m.deleted.Load() != 1 {
		t.Errorf("deleted %d units, want the one the node ran", m.deleted.Load())
	}
	rev, err := RemovedFrom(dir)
	if err != nil || rev == nil || rev.By != "n2" || rev.Reason != "decommissioned" {
		t.Errorf("marker = %+v, %v; want the revocation by n2", rev, err)
	}
}

func TestAnotherNodesRevocationLeavesThisOneRunning(t *testing.T) {
	defer func(d time.Duration) { removalPoll = d }(removalPoll)
	removalPoll = 50 * time.Millisecond
	a, _ := newRemovableAgent(t, t.TempDir())
	revoke(t, a.store, "n2")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run returned %v, want a clean stop", err)
	}
}

func TestAnAgentOnARemovedNodeRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	if err := writeRemovedMarker(dir, &nodelc.Revocation{NodeID: "n1", By: "n2"}); err != nil {
		t.Fatal(err)
	}
	a, _ := newRemovableAgent(t, dir)
	err := runUntilStopped(t, a)
	if !errors.Is(err, ErrRemoved) {
		t.Fatalf("Run returned %v, want ErrRemoved", err)
	}
}

func TestANodeNeverRemovedHasNoMarker(t *testing.T) {
	if rev, err := RemovedFrom(t.TempDir()); rev != nil || err != nil {
		t.Errorf("RemovedFrom = %+v, %v; want nothing", rev, err)
	}
}
