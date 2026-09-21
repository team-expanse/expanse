package drbd

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func newAllocator(t *testing.T) *Allocator {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "alloc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewAllocator(st, Range{Lo: 10, Hi: 19}, Range{Lo: 7800, Hi: 7809})
}

func mustAllocate(t *testing.T, a *Allocator, name string) Allocation {
	t.Helper()
	al, err := a.Allocate(context.Background(), name)
	if err != nil {
		t.Fatalf("Allocate(%s): %v", name, err)
	}
	return al
}

func wantKind(t *testing.T, err error, kind experrors.Kind) {
	t.Helper()
	if experrors.KindOf(err) != kind {
		t.Errorf("want kind %v, got %v", kind, err)
	}
}

func TestAllocateTakesLowestFreeMinorAndPort(t *testing.T) {
	a := newAllocator(t)
	first, second := mustAllocate(t, a, "vol-a"), mustAllocate(t, a, "vol-b")
	if first.Minor != 10 || first.Port != 7800 || second.Minor != 11 || second.Port != 7801 {
		t.Errorf("got %+v then %+v", first, second)
	}
}

func TestAllocateIsIdempotent(t *testing.T) {
	a := newAllocator(t)
	first := mustAllocate(t, a, "vol-a")
	mustAllocate(t, a, "vol-b")
	again := mustAllocate(t, a, "vol-a")
	if again.Minor != first.Minor || again.Port != first.Port {
		t.Errorf("re-allocation moved the volume: %+v -> %+v", first, again)
	}
}

func TestAllocateNeverCollidesUnderConcurrency(t *testing.T) {
	a := newAllocator(t)
	const n = 10
	results := make([]Allocation, n)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			al, err := a.Allocate(context.Background(), fmt.Sprintf("vol-%d", i))
			if err != nil {
				t.Errorf("vol-%d: %v", i, err)
			}
			results[i] = al
		}()
	}
	wg.Wait()
	minors, ports := map[int]bool{}, map[int]bool{}
	for _, al := range results {
		if minors[al.Minor] || ports[al.Port] {
			t.Errorf("collision on %+v", al)
		}
		minors[al.Minor], ports[al.Port] = true, true
	}
	if len(minors) != n {
		t.Errorf("want %d distinct minors, got %d", n, len(minors))
	}
}

func TestReleaseFreesTheClaims(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	mustAllocate(t, a, "vol-a")
	mustAllocate(t, a, "vol-b")
	if err := a.Release(ctx, "vol-a"); err != nil {
		t.Fatal(err)
	}
	if got := mustAllocate(t, a, "vol-c"); got.Minor != 10 || got.Port != 7800 {
		t.Errorf("released numbers not reused lowest-first: %+v", got)
	}
	if _, err := a.Get(ctx, "vol-a"); experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("released volume still present: %v", err)
	}
	wantKind(t, a.Release(ctx, "vol-a"), experrors.KindNotFound)
}

func TestAllocateExhaustion(t *testing.T) {
	a := newAllocator(t)
	for i := 0; i < 10; i++ {
		mustAllocate(t, a, fmt.Sprintf("vol-%d", i))
	}
	_, err := a.Allocate(context.Background(), "vol-extra")
	wantKind(t, err, experrors.KindResourceExhausted)
}

func TestAllocateRejectsBadName(t *testing.T) {
	a := newAllocator(t)
	for _, name := range []string{"", "-r0", "a/b", "a b"} {
		_, err := a.Allocate(context.Background(), name)
		wantKind(t, err, experrors.KindInvalid)
	}
}

func TestAssignNodeIDIsSequentialAndIdempotent(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	mustAllocate(t, a, "vol-a")
	for i, host := range []string{"n1", "n2", "n3", "n2"} {
		id, err := a.AssignNodeID(ctx, "vol-a", host)
		want := map[int]int{0: 0, 1: 1, 2: 2, 3: 1}[i]
		if err != nil || id != want {
			t.Errorf("%s: got id %d err %v, want %d", host, id, err, want)
		}
	}
}

func TestAssignNodeIDUnknownVolume(t *testing.T) {
	_, err := newAllocator(t).AssignNodeID(context.Background(), "nope", "n1")
	wantKind(t, err, experrors.KindNotFound)
}

func TestAssignNodeIDConcurrentHostsGetDistinctIDs(t *testing.T) {
	a := newAllocator(t)
	mustAllocate(t, a, "vol-a")
	ids := make([]int, 6)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := a.AssignNodeID(context.Background(), "vol-a", fmt.Sprintf("n%d", i))
			if err != nil {
				t.Errorf("n%d: %v", i, err)
			}
			ids[i] = id
		}()
	}
	wg.Wait()
	seen := map[int]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate node-id %d in %v", id, ids)
		}
		seen[id] = true
	}
}

func TestRebuildGetsAFreshIDAndTheDeadOneStaysRetired(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	mustAllocate(t, a, "vol-a")
	for _, h := range []string{"n1", "n2", "n3"} {
		a.AssignNodeID(ctx, "vol-a", h)
	}
	dead, err := a.RetireNode(ctx, "vol-a", "n3")
	if err != nil || dead != 2 {
		t.Fatalf("RetireNode = %d, %v", dead, err)
	}
	fresh, err := a.AssignNodeID(ctx, "vol-a", "n4")
	if err != nil || fresh == dead {
		t.Errorf("replacement got id %d (dead was %d), err %v", fresh, dead, err)
	}
	al, _ := a.Get(ctx, "vol-a")
	if len(al.Retired) != 1 || al.Retired[0] != dead {
		t.Errorf("dead id not recorded as retired: %+v", al)
	}
	if _, present := al.NodeIDs["n3"]; present {
		t.Errorf("retired host still mapped: %+v", al)
	}
}

func TestRetireUnknownHost(t *testing.T) {
	a := newAllocator(t)
	mustAllocate(t, a, "vol-a")
	_, err := a.RetireNode(context.Background(), "vol-a", "ghost")
	wantKind(t, err, experrors.KindNotFound)
}

func TestConfirmForgottenMovesRetiredToReusable(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	mustAllocate(t, a, "vol-a")
	a.AssignNodeID(ctx, "vol-a", "n1")
	dead, _ := a.RetireNode(ctx, "vol-a", "n1")
	if err := a.ConfirmForgotten(ctx, "vol-a", dead); err != nil {
		t.Fatal(err)
	}
	al, _ := a.Get(ctx, "vol-a")
	if len(al.Retired) != 0 || len(al.Forgotten) != 1 || al.Forgotten[0] != dead {
		t.Errorf("want id %d forgotten, got %+v", dead, al)
	}
	wantKind(t, a.ConfirmForgotten(ctx, "vol-a", 99), experrors.KindNotFound)
}

func fillNodeIDs(t *testing.T, a *Allocator) {
	t.Helper()
	mustAllocate(t, a, "vol-a")
	for i := 0; i <= maxNodeID; i++ {
		if _, err := a.AssignNodeID(context.Background(), "vol-a", fmt.Sprintf("h%d", i)); err != nil {
			t.Fatalf("id %d: %v", i, err)
		}
	}
}

func TestNodeIDCeilingNeedsConfirmedForgetToRecycle(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	fillNodeIDs(t, a)
	_, err := a.AssignNodeID(ctx, "vol-a", "extra")
	wantKind(t, err, experrors.KindResourceExhausted)

	dead, _ := a.RetireNode(ctx, "vol-a", "h31")
	_, err = a.AssignNodeID(ctx, "vol-a", "extra")
	wantKind(t, err, experrors.KindResourceExhausted) // retired, not yet forgotten

	if err := a.ConfirmForgotten(ctx, "vol-a", dead); err != nil {
		t.Fatal(err)
	}
	if id, err := a.AssignNodeID(ctx, "vol-a", "extra"); err != nil || id != dead {
		t.Errorf("want recycled id %d, got %d, %v", dead, id, err)
	}
}

func TestNodeIDRecyclesLowestForgottenAfterCeiling(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	fillNodeIDs(t, a)
	for _, h := range []string{"h7", "h3"} {
		id, _ := a.RetireNode(ctx, "vol-a", h)
		if err := a.ConfirmForgotten(ctx, "vol-a", id); err != nil {
			t.Fatal(err)
		}
	}
	id, err := a.AssignNodeID(ctx, "vol-a", "fresh")
	if err != nil || id != 3 {
		t.Errorf("want lowest forgotten id 3, got %d, %v", id, err)
	}
}

func TestNeverUsedIDsAreExhaustedBeforeAnyRecycling(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	mustAllocate(t, a, "vol-a")
	a.AssignNodeID(ctx, "vol-a", "n1")
	dead, _ := a.RetireNode(ctx, "vol-a", "n1")
	a.ConfirmForgotten(ctx, "vol-a", dead)
	id, _ := a.AssignNodeID(ctx, "vol-a", "n2")
	if id == dead {
		t.Errorf("recycled id %d while never-used ids remain", dead)
	}
}

func retireOf(t *testing.T, a *Allocator, hosts ...string) int {
	t.Helper()
	ctx := context.Background()
	mustAllocate(t, a, "vol-a")
	for _, h := range hosts {
		if _, err := a.AssignNodeID(ctx, "vol-a", h); err != nil {
			t.Fatal(err)
		}
	}
	id, err := a.RetireNode(ctx, "vol-a", hosts[len(hosts)-1])
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAckForgottenWaitsForEverySurvivor(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	dead := retireOf(t, a, "n1", "n2", "n3")
	for i, host := range []string{"n1", "n2"} {
		if err := a.AckForgotten(ctx, "vol-a", dead, host); err != nil {
			t.Fatal(err)
		}
		al, _ := a.Get(ctx, "vol-a")
		forgotten := len(al.Forgotten) == 1
		if forgotten != (i == 1) {
			t.Errorf("after %d of 2 acks: forgotten=%v, %+v", i+1, forgotten, al)
		}
	}
	al, _ := a.Get(ctx, "vol-a")
	if len(al.Retired) != 0 || len(al.Acks) != 0 {
		t.Errorf("bookkeeping left behind: %+v", al)
	}
}

func TestAckForgottenIsIdempotent(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	dead := retireOf(t, a, "n1", "n2", "n3")
	for _, host := range []string{"n1", "n1", "n1"} {
		if err := a.AckForgotten(ctx, "vol-a", dead, host); err != nil {
			t.Fatal(err)
		}
	}
	if al, _ := a.Get(ctx, "vol-a"); len(al.Forgotten) != 0 {
		t.Errorf("one survivor acking three times completed the forget: %+v", al)
	}
	a.AckForgotten(ctx, "vol-a", dead, "n2")
	if err := a.AckForgotten(ctx, "vol-a", dead, "n2"); err != nil {
		t.Errorf("ack after completion should be a no-op, got %v", err)
	}
}

func TestAckForgottenAlsoNeedsAReplacementAddedMeanwhile(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	dead := retireOf(t, a, "n1", "n2", "n3")
	a.AssignNodeID(ctx, "vol-a", "n4")
	a.AckForgotten(ctx, "vol-a", dead, "n1")
	a.AckForgotten(ctx, "vol-a", dead, "n2")
	if al, _ := a.Get(ctx, "vol-a"); len(al.Forgotten) != 0 {
		t.Errorf("forgotten before the new member acked: %+v", al)
	}
	a.AckForgotten(ctx, "vol-a", dead, "n4")
	if al, _ := a.Get(ctx, "vol-a"); len(al.Forgotten) != 1 {
		t.Errorf("not forgotten after every member acked: %+v", al)
	}
}

func TestAckForgottenRejectsStrangers(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	dead := retireOf(t, a, "n1", "n2")
	wantKind(t, a.AckForgotten(ctx, "vol-a", dead, "ghost"), experrors.KindInvalid)
	wantKind(t, a.AckForgotten(ctx, "vol-a", 25, "n1"), experrors.KindNotFound)
	wantKind(t, a.AckForgotten(ctx, "nope", dead, "n1"), experrors.KindNotFound)
}

func TestForgottenIDIsReusableAfterAcks(t *testing.T) {
	a, ctx := newAllocator(t), context.Background()
	fillNodeIDs(t, a)
	dead, _ := a.RetireNode(ctx, "vol-a", "h5")
	for i := 0; i <= maxNodeID; i++ {
		if i != 5 {
			a.AckForgotten(ctx, "vol-a", dead, fmt.Sprintf("h%d", i))
		}
	}
	if id, err := a.AssignNodeID(ctx, "vol-a", "fresh"); err != nil || id != dead {
		t.Errorf("want recycled id %d, got %d, %v", dead, id, err)
	}
}

func TestVolumeStartsUninitializedAndStaysInitializedOnceMarked(t *testing.T) {
	a := newAllocator(t)
	ctx := context.Background()
	if mustAllocate(t, a, "vol-a").Initialized {
		t.Fatal("a new volume must not start out initialized")
	}
	for range 2 { // marking is idempotent
		if err := a.MarkInitialized(ctx, "vol-a"); err != nil {
			t.Fatal(err)
		}
	}
	al, err := a.Get(ctx, "vol-a")
	if err != nil || !al.Initialized {
		t.Fatalf("got %+v, %v", al, err)
	}
	if again := mustAllocate(t, a, "vol-a"); !again.Initialized {
		t.Error("Allocate on an existing volume dropped the marker")
	}
}

func TestMarkInitializedIsPerVolumeAndNeedsTheVolume(t *testing.T) {
	a := newAllocator(t)
	mustAllocate(t, a, "vol-a")
	other := mustAllocate(t, a, "vol-b")
	if err := a.MarkInitialized(context.Background(), "vol-a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(context.Background(), "vol-b"); got.Initialized || other.Initialized {
		t.Error("marking one volume marked another")
	}
	wantKind(t, a.MarkInitialized(context.Background(), "ghost"), experrors.KindNotFound)
}
