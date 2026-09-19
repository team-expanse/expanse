package oplog

import "testing"

func TestSpansSuperseded(t *testing.T) {
	sp := NewSpans(map[uint64]Span{
		1: {Offset: 0, Length: 4096},
		2: {Offset: 0, Length: 4096},     // fully overwrites 1
		3: {Offset: 20480, Length: 4096}, // disjoint
		4: {Offset: 12000, Length: 100},  // straddles a page boundary
		5: {Offset: 12200, Length: 4000},
		6: {Offset: 0, Length: 0}, // flush marker
	})
	for seq, want := range map[uint64]bool{1: true, 2: false, 3: false, 4: true, 5: false, 6: false} {
		if got := sp.Superseded(seq); got != want {
			t.Errorf("Superseded(%d) = %v, want %v", seq, got, want)
		}
	}
	var none *Spans
	if none.Superseded(1) {
		t.Error("unknown spans must never claim supersession (torn-zvol detection stays on)")
	}
}
