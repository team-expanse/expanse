package volume

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/expanse/expanse/internal/storage"
)

func (r *nodeRig) markGone(t *testing.T, node, id string) {
	t.Helper()
	if err := storage.PutGone(context.Background(), r.st, node, id); err != nil {
		t.Fatal(err)
	}
}

func (r *nodeRig) goneFor(t *testing.T, node string) []string {
	t.Helper()
	ids, err := storage.ListGone(context.Background(), r.st, node)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestAGoneMarkRemovesTheReplicaAndIsCleared(t *testing.T) {
	r := newNodeRig(t)
	r.conv.present[vol] = true // the volume's records are gone; only the replica is left
	r.markGone(t, "n1", vol)
	r.mustSync(t)
	if !slices.Equal(r.conv.removed, []string{vol}) {
		t.Errorf("removed %v, want [%s]", r.conv.removed, vol)
	}
	if got := r.goneFor(t, "n1"); len(got) != 0 {
		t.Errorf("mark left behind: %v", got)
	}
}

func TestAGoneMarkForAReplicaThatIsAlreadyGoneIsJustCleared(t *testing.T) {
	r := newNodeRig(t)
	r.markGone(t, "n1", vol)
	r.mustSync(t)
	if len(r.conv.removed) != 0 {
		t.Errorf("removed %v from a node that held nothing", r.conv.removed)
	}
	if got := r.goneFor(t, "n1"); len(got) != 0 {
		t.Errorf("mark left behind: %v", got)
	}
}

func TestAnotherNodesGoneMarkIsLeftAlone(t *testing.T) {
	r := newNodeRig(t)
	r.conv.present[vol] = true
	r.markGone(t, "n2", vol)
	r.mustSync(t)
	if len(r.conv.removed) != 0 {
		t.Errorf("n1 removed %v on n2's mark", r.conv.removed)
	}
	if got := r.goneFor(t, "n2"); !slices.Equal(got, []string{vol}) {
		t.Errorf("n2's mark = %v", got)
	}
}

func TestAGoneMarkStopsTheLeaderBeforeRemoving(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.mustSync(t)
	waitStarted(t, r.lead)
	r.setState(t, storage.StateDeleting)
	r.markGone(t, "n1", vol)
	r.mustSync(t)
	if r.lead.isRunning(vol) {
		t.Error("still leading a volume marked gone")
	}
}

func TestAFailedRemovalKeepsTheGoneMark(t *testing.T) {
	r := newNodeRig(t)
	r.conv.present[vol] = true
	r.conv.removeErr = errors.New("lvremove: busy")
	r.markGone(t, "n1", vol)
	if err := r.sync(t); err == nil {
		t.Fatal("a failed removal must be reported")
	}
	if got := r.goneFor(t, "n1"); !slices.Equal(got, []string{vol}) {
		t.Errorf("mark = %v; it must stay until the replica is gone", got)
	}
}
