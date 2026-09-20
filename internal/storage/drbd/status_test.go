package drbd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "drbd", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parse(t *testing.T, name string) *Status {
	t.Helper()
	st, err := ParseStatus("r0", fixture(t, name))
	if err != nil {
		t.Fatalf("ParseStatus(%s): %v", name, err)
	}
	return st
}

func peer(t *testing.T, st *Status, name string) Peer {
	t.Helper()
	for _, p := range st.Peers {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no peer %q in %+v", name, st.Peers)
	return Peer{}
}

func TestHealthyPrimary(t *testing.T) {
	st := parse(t, "healthy-primary.n1.json")
	if st.Name != "r0" || st.NodeID != 0 || st.Role != RolePrimary {
		t.Fatalf("resource = %+v", st)
	}
	if len(st.Volumes) != 1 || st.Volumes[0].DiskState != DiskUpToDate || !st.Volumes[0].Quorum {
		t.Fatalf("volumes = %+v", st.Volumes)
	}
	if len(st.Peers) != 2 {
		t.Fatalf("peers = %+v", st.Peers)
	}
	for _, p := range st.Peers {
		if p.Connection != ConnConnected || p.Role != RoleSecondary || p.Volumes[0].DiskState != DiskUpToDate {
			t.Errorf("peer %s = %+v", p.Name, p)
		}
		if p.Resyncing() {
			t.Errorf("peer %s reported resyncing", p.Name)
		}
	}
	if !st.HasQuorum() {
		t.Error("healthy resource lost quorum")
	}
}

func TestHealthySecondaryReportsThePrimaryPeer(t *testing.T) {
	st := parse(t, "healthy-secondary.n2.json")
	if st.Role != RoleSecondary || st.NodeID != 1 {
		t.Fatalf("resource = %+v", st)
	}
	if got := peer(t, st, "n1").Role; got != RolePrimary {
		t.Errorf("n1 role = %q, want Primary", got)
	}
}

func TestDegradedKeepsQuorumWithOnePeerConnecting(t *testing.T) {
	st := parse(t, "degraded.n1.json")
	lost := peer(t, st, "n3")
	if lost.Connection != ConnConnecting || lost.Volumes[0].DiskState != DiskUnknown {
		t.Fatalf("lost peer = %+v", lost)
	}
	if peer(t, st, "n2").Connection != ConnConnected || !st.HasQuorum() {
		t.Fatalf("survivor/quorum wrong: %+v", st)
	}
}

func TestSyncingSourceAndTarget(t *testing.T) {
	src := peer(t, parse(t, "syncing-source.n1.json"), "n3")
	if !src.Resyncing() || src.Volumes[0].Replication != ReplSyncSource || src.Volumes[0].DiskState != DiskInconsistent {
		t.Fatalf("source view = %+v", src)
	}
	if v := src.Volumes[0]; v.OutOfSyncKiB != 62976 || v.PercentInSync != 75.97 {
		t.Errorf("progress = %+v", v)
	}
	tgt := parse(t, "syncing-target.n3.json")
	if got := tgt.Volumes[0].DiskState; got != DiskInconsistent {
		t.Errorf("target disk = %q, want Inconsistent", got)
	}
	if !peer(t, tgt, "n1").Resyncing() {
		t.Error("target does not see its peer resyncing")
	}
}

func TestQuorumLost(t *testing.T) {
	st := parse(t, "quorum-lost.n1.json")
	if st.HasQuorum() {
		t.Fatal("quorum-lost fixture reports quorum")
	}
	if st.Role != RolePrimary {
		t.Errorf("role = %q", st.Role)
	}
}

func TestSplitBrainAppearsAsStandAloneOnBothSides(t *testing.T) {
	for _, name := range []string{"split-brain.n1.json", "split-brain.n2.json"} {
		st, err := ParseStatus("r1", fixture(t, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Peers) != 1 || st.Peers[0].Connection != ConnStandAlone || st.Peers[0].Role != RoleUnknown {
			t.Errorf("%s peers = %+v", name, st.Peers)
		}
	}
}

func TestParseStatusRejectsBadInput(t *testing.T) {
	cases := map[string]struct {
		in   string
		kind experrors.Kind
	}{
		"empty array is an unknown resource": {"[\n]\n", experrors.KindNotFound},
		"not json":                           {"resource r0 {", experrors.KindInternal},
		"empty output":                       {"", experrors.KindInternal},
		"truncated":                          {`[{"name": "r0", "devices": [`, experrors.KindInternal},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseStatus("r0", []byte(tc.in))
			if experrors.KindOf(err) != tc.kind {
				t.Fatalf("kind = %q, want %q (%v)", experrors.KindOf(err), tc.kind, err)
			}
			if err != nil && !strings.Contains(err.Error(), "r0") {
				t.Errorf("error does not name the resource: %v", err)
			}
		})
	}
}

func TestStatusRunsDrbdsetupJSON(t *testing.T) {
	f := newFake(string(fixture(t, "healthy-primary.n1.json")), "", 0)
	st, err := f.Status(context.Background(), "r0")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"drbdsetup", "status", "r0", "--json", "--verbose", "--statistics"}
	if len(f.calls) != 1 || len(f.calls[0]) != len(want) {
		t.Fatalf("calls = %v", f.calls)
	}
	for i := range want {
		if f.calls[0][i] != want[i] {
			t.Fatalf("calls = %v, want %v", f.calls, want)
		}
	}
	if st.Role != RolePrimary {
		t.Errorf("role = %q", st.Role)
	}
}

func TestPausedResyncStillCountsAsResyncing(t *testing.T) {
	for _, r := range []Replication{ReplPausedSyncS, ReplPausedSyncT} {
		p := Peer{Volumes: []PeerVolume{{Replication: r}}}
		if !p.Resyncing() {
			t.Errorf("%s not reported as resyncing", r)
		}
	}
	if (Peer{Volumes: []PeerVolume{{Replication: ReplEstablished}}}).Resyncing() {
		t.Error("Established reported as resyncing")
	}
}
