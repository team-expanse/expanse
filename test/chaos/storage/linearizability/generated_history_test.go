package linearizability

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// genRegisterHistory simulates a register that is linearizable by construction: each op
// takes effect at a random point inside its interval. A share of writes are indeterminate
// (applied at some later point, or never) and a share of reads are corrupted, which
// usually — not always — breaks linearizability. Timestamps are all distinct because the
// two checkers deliberately differ on exact ties (closed vs. half-open intervals).
func genRegisterHistory(rng *rand.Rand, corrupt bool) []Op {
	const clients = 3
	n := 2 + rng.Intn(11)
	type pt struct {
		at float64
		i  int
	}
	ops := make([]Op, n)
	points := make([]pt, 0, n)
	var clock [clients]int64
	nextVal := uint64(0)
	horizon := int64(0)
	for i := range ops {
		c := i % clients
		call := clock[c] + 1 + rng.Int63n(3)
		ret := call + 1 + rng.Int63n(4)
		clock[c] = ret
		if ret > horizon {
			horizon = ret
		}
		// scaled so every timestamp is distinct: calls even, returns odd
		o := Op{ID: i + 1, Client: c, Call: call*1000 + 2*int64(i), Return: ret*1000 + 2*int64(i) + 1}
		o.Kind = Read
		if rng.Intn(2) == 0 {
			o.Kind = Write
			nextVal++
			o.Value = nextVal
			if rng.Intn(4) == 0 {
				o.Indeterminate, o.Return = true, 0
			}
		}
		ops[i] = o
	}
	for i, o := range ops {
		end := float64(o.Return)
		if o.Indeterminate {
			end = float64(horizon+1) * 1000 // may take effect long after the client gave up
			if rng.Intn(2) == 0 {
				continue // never applied
			}
		}
		points = append(points, pt{float64(o.Call) + rng.Float64()*(end-float64(o.Call)), i})
	}
	sort.Slice(points, func(x, y int) bool { return points[x].at < points[y].at })
	cur := uint64(0)
	for _, p := range points {
		if o := &ops[p.i]; o.Kind == Write {
			cur = o.Value
		} else {
			o.Value = cur
		}
	}
	if corrupt {
		var reads []int
		for i, o := range ops {
			if o.Kind == Read {
				reads = append(reads, i)
			}
		}
		if len(reads) > 0 {
			ops[reads[rng.Intn(len(reads))]].Value = uint64(rng.Intn(int(nextVal) + 2))
		}
	}
	return ops
}

// Uncorrupted histories are linearizable by construction, so the checker must accept
// every one; corrupted ones are judged with and without the unobserved-write prune,
// which must never change a verdict.
func TestGeneratedHistories(t *testing.T) {
	const histories = 30000
	rng := rand.New(rand.NewSource(20260920))
	var violations int
	for h := 0; h < histories; h++ {
		corrupt := rng.Intn(3) == 0
		ops := genRegisterHistory(rng, corrupt)
		got, err := CheckRegister(ops, testTimeout)
		if err != nil {
			t.Fatalf("history %d: %v", h, err)
		}
		if !corrupt && !got.OK {
			t.Fatalf("history %d is linearizable by construction but was rejected:\n%s", h, dump(ops))
		}
		unpruned, err := checkRegister(ops, testTimeout)
		if err != nil {
			t.Fatalf("history %d unpruned: %v", h, err)
		}
		if unpruned.OK != got.OK {
			t.Fatalf("history %d: the prune changed the verdict (pruned %v, unpruned %v):\n%s", h, got.OK, unpruned.OK, dump(ops))
		}
		if !got.OK {
			violations++
		}
	}
	if violations < histories/20 {
		t.Fatalf("only %d of %d histories were violations: the generator is too tame to prove much", violations, histories)
	}
}

func dump(ops []Op) string {
	s := ""
	for _, o := range ops {
		s += fmt.Sprintf("  %+v\n", o)
	}
	return s
}
