package agent

// Tests for the UI VIP's candidate extraction (A3): every non-witness
// cluster member is a candidate, unlike a block's ready-replica count.

import (
	"context"
	"log/slog"
	"path/filepath"
	"sort"
	"testing"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func putNodeRecord(t *testing.T, st store.Store, id, role string) {
	t.Helper()
	v := `{"id":"` + id + `","role":"` + role + `"}`
	if _, err := st.Put(context.Background(), store.Key("/nodes/"+id), []byte(v)); err != nil {
		t.Fatalf("put node record %s: %v", id, err)
	}
}

func TestUIVIPCandidatesExcludesWitnesses(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	defer st.Close()

	putNodeRecord(t, st, "n1", "voter")
	putNodeRecord(t, st, "n2", "voter")
	putNodeRecord(t, st, "n3", "witness")
	// A status sub-key must not be mistaken for a node record.
	if _, err := st.Put(context.Background(), "/nodes/n1/status", []byte(`{"phase":"ready"}`)); err != nil {
		t.Fatalf("put status: %v", err)
	}

	a := &Agent{store: st, logger: slog.Default()}
	got := a.uiVIPCandidates()

	var ids []string
	for _, c := range got {
		ids = append(ids, c.NodeID)
		if c.ReadyReplicas != 1 {
			t.Errorf("candidate %s: ReadyReplicas = %d, want 1 (membership is the readiness signal)", c.NodeID, c.ReadyReplicas)
		}
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "n1" || ids[1] != "n2" {
		t.Fatalf("candidates = %v, want [n1 n2] (n3 is a witness, /nodes/n1/status is not a node record)", ids)
	}
}

func TestUIVIPCandidatesEmptyStore(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	defer st.Close()

	a := &Agent{store: st, logger: slog.Default()}
	if got := a.uiVIPCandidates(); len(got) != 0 {
		t.Fatalf("candidates = %v, want none", got)
	}
}
