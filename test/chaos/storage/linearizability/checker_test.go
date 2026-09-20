package linearizability

import (
	"testing"
	"time"
)

const testTimeout = 10 * time.Second

// w/r build ops on key 0; times are abstract ticks.
func w(id int, val uint64, call, ret int64) Op {
	return Op{ID: id, Kind: Write, Value: val, Call: call, Return: ret}
}

func wInfo(id int, val uint64, call int64) Op {
	return Op{ID: id, Kind: Write, Value: val, Call: call, Indeterminate: true}
}

func r(id int, val uint64, call, ret int64) Op {
	return Op{ID: id, Kind: Read, Value: val, Call: call, Return: ret}
}

type registerCase struct {
	name string
	ops  []Op
	ok   bool
}

var registerCases = []registerCase{
	{"empty", nil, true},
	{"initial zero read", []Op{r(1, 0, 1, 2)}, true},
	{"sequential write then read", []Op{w(1, 7, 1, 2), r(2, 7, 3, 4)}, true},
	{"acked write lost", []Op{w(1, 7, 1, 2), r(2, 0, 3, 4)}, false},
	{"stale read after newer acked write", []Op{
		w(1, 7, 1, 2), w(2, 8, 3, 4), r(3, 7, 5, 6),
	}, false},
	{"concurrent read may see old or new", []Op{
		w(1, 7, 1, 10), r(2, 0, 2, 3), r(3, 7, 4, 5),
	}, true},
	{"read from a future write", []Op{r(1, 7, 1, 2), w(2, 7, 3, 4)}, false},
	{"read of a value nobody wrote", []Op{r(1, 99, 1, 2)}, false},
	{"indeterminate write may surface later", []Op{
		w(1, 7, 1, 2), wInfo(2, 8, 3), r(3, 8, 10, 11),
	}, true},
	{"indeterminate write may never surface", []Op{
		w(1, 7, 1, 2), wInfo(2, 8, 3), r(3, 7, 10, 11),
	}, true},
	{"indeterminate write cannot flip-flop", []Op{
		w(1, 7, 1, 2), wInfo(2, 8, 3), r(3, 8, 10, 11), r(4, 7, 12, 13),
	}, false},
	{"overlapping writes either order", []Op{
		w(1, 7, 1, 10), w(2, 8, 2, 9), r(3, 7, 11, 12),
	}, true},
	{"two reads disagree with any single order", []Op{
		w(1, 7, 1, 20), w(2, 8, 1, 20), r(3, 7, 2, 3), r(4, 8, 4, 5), r(5, 7, 6, 7),
	}, false},
}

func TestCheckRegister(t *testing.T) {
	for _, tc := range registerCases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := CheckRegister(tc.ops, testTimeout)
			if err != nil {
				t.Fatal(err)
			}
			if res.OK != tc.ok {
				t.Fatalf("OK=%v want %v (%s)", res.OK, tc.ok, res.Reason)
			}
		})
	}
}

// A search that cannot finish must be an error, never a pass.
func TestCheckRegisterTimeoutIsNeverAPass(t *testing.T) {
	var ops []Op
	for i := 1; i <= 12; i++ {
		ops = append(ops, wInfo(i, uint64(i), 1), r(20+i, uint64(i), 2, 3))
	}
	ops = append(ops, r(99, 500, 4, 5))
	if res, err := CheckRegister(ops, time.Nanosecond); err == nil && res.OK {
		t.Fatal("an unfinished search reported linearizable")
	}
}

// Indeterminate writes nobody read must not blow up the search.
func TestUnobservedIndeterminateWritesAreCheap(t *testing.T) {
	ops := []Op{w(1, 7, 1, 2)}
	for i := 0; i < 40; i++ {
		ops = append(ops, wInfo(10+i, uint64(100+i), 1))
	}
	ops = append(ops, r(99, 7, 3, 4))
	res, err := CheckRegister(ops, testTimeout)
	if err != nil || !res.OK {
		t.Fatalf("res=%+v err=%v, want linearizable", res, err)
	}
}

// Pruning must not hide a violation: a lost acked write stays a failure.
func TestPruningKeepsAckedWriteLossDetectable(t *testing.T) {
	ops := []Op{w(1, 7, 1, 2), wInfo(2, 8, 1), r(3, 0, 3, 4)}
	res, err := CheckRegister(ops, testTimeout)
	if err != nil || res.OK {
		t.Fatalf("res=%+v err=%v, want a violation", res, err)
	}
}

func TestCheckHistoryGroupsByKey(t *testing.T) {
	h := []Op{
		{ID: 1, Key: 1, Kind: Write, Value: 7, Call: 1, Return: 2},
		{ID: 2, Key: 2, Kind: Read, Value: 0, Call: 1, Return: 2},
		{ID: 3, Key: 1, Kind: Read, Value: 0, Call: 3, Return: 4}, // key 1 lost its write
	}
	res, err := CheckHistory(h, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Key != 1 {
		t.Fatalf("want violation on key 1, got %+v", res)
	}
}
