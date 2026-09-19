package runtime

import (
	"testing"

	pb "github.com/expanse/expanse/proto"
)

func claim(seq, off uint64, length uint32) *pb.SeqInfo {
	return &pb.SeqInfo{Seq: seq, Offset: off, Length: length}
}

// The watchdog compares an old op's payload with the primary's CURRENT
// bytes, which only makes sense if no later op rewrote that range —
// ext4 rewrites its journal and group metadata constantly.
func TestSupersededByLaterOp(t *testing.T) {
	claims := []*pb.SeqInfo{
		claim(7, 1000, 100), // [1000,1100)
		claim(8, 5000, 100),
		claim(9, 1099, 10),  // [1099,1109): overlaps op 7's last byte
		claim(10, 0, 0),     // flush marker: no range
		claim(11, 1100, 50), // [1100,1150)
	}
	cases := []struct {
		name     string
		seq, off uint64
		length   uint64
		want     bool
	}{
		{"overwritten by a later op", 7, 1000, 100, true},
		{"only unrelated later ops", 8, 5000, 100, false},
		{"latest op is never superseded", 11, 1100, 50, false},
		{"flush claims carry no range", 9, 0, 10, false},
		{"range ending where a later op starts", 6, 900, 100, false},
		{"later op starting at the exclusive end", 9, 1000, 100, false},
	}
	for _, c := range cases {
		if got := supersededByLaterOp(claims, c.seq, c.off, c.length); got != c.want {
			t.Errorf("%s: supersededByLaterOp(seq=%d off=%d len=%d) = %v, want %v",
				c.name, c.seq, c.off, c.length, got, c.want)
		}
	}
}

// Two live copies read a moment apart can disagree while a write to the
// range is in flight; real divergence persists. Step down only on the
// second consecutive sighting for the same op.
func TestMismatchConfirmer(t *testing.T) {
	var m mismatchConfirmer
	if m.observe(7, true) {
		t.Fatal("first mismatch must not confirm")
	}
	if !m.observe(7, true) {
		t.Fatal("same op mismatching again must confirm")
	}

	m = mismatchConfirmer{}
	m.observe(7, true)
	m.observe(7, false) // transient: the copies converged
	if m.observe(7, true) {
		t.Fatal("a clean check in between must reset confirmation")
	}

	m = mismatchConfirmer{}
	m.observe(7, true)
	if m.observe(9, true) {
		t.Fatal("a mismatch on a different op is a first sighting, not a confirmation")
	}
}
