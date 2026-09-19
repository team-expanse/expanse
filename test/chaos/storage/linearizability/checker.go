// Package linearizability is the Phase 06 T21 Jepsen-style history
// checker for exvol block volumes. Every 4 KiB block is a read/write
// register; concurrent clients record (call, return, op, result)
// histories while a nemesis injects faults, and CheckHistory then
// decides — exactly, per block — whether some total order consistent
// with real time explains every observed read (Wing & Gong search with
// state memoisation).
//
// The scenario (run_test.go) drives the in-process exvol harness
// (test/chaos/exvol) with a per-link fault layer: kill / partition /
// SIGSTOP-pause the primary or a secondary, or slow a node's links, one
// fault at a time with recovery in between. Writes are write+flush; a
// write whose outcome is unknown is "indeterminate" and may or may not
// have taken effect. Every block is finally read back through the settled
// primary and all replicas must hold identical bytes.
//
//	go test ./test/chaos/storage/linearizability/                 # 45 s (default)
//	CHAOS_DURATION=5m go test ...                                 # any length
//	RUN_CHAOS=1 go test -timeout 90m ./test/chaos/storage/linearizability/   # 1 h nightly (make chaos-storage)
//
// Any acked-write loss is a release blocker.
package linearizability

import (
	"fmt"
	"sort"
	"strings"
)

// Kind is the operation type.
type Kind uint8

const (
	Write Kind = iota
	Read
)

// Op is one client operation on one block. Value is the token written,
// or the token a read observed (0 = the never-written zero block).
type Op struct {
	ID     int
	Client int
	Key    int
	Kind   Kind
	Value  uint64
	Call   int64 // ns since run start
	Return int64
	// Indeterminate marks a write whose outcome is unknown (error,
	// timeout): it may have taken effect at any time after Call, or never.
	Indeterminate bool
}

// Result is a checker verdict; on failure Key/Reason locate the block.
type Result struct {
	OK     bool
	Key    int
	Reason string
}

// CheckHistory groups ops by block and checks each independently
// (registers compose: a history is linearizable iff every key's is).
func CheckHistory(ops []Op, budget int) (Result, error) {
	byKey := map[int][]Op{}
	for _, o := range ops {
		byKey[o.Key] = append(byKey[o.Key], o)
	}
	keys := make([]int, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		res, err := CheckRegister(byKey[k], budget)
		if err != nil {
			return Result{Key: k}, fmt.Errorf("key %d: %w", k, err)
		}
		if !res.OK {
			res.Key = k
			return res, nil
		}
	}
	return Result{OK: true}, nil
}

// CheckRegister decides linearizability of one register whose initial
// value is 0. budget bounds search steps; exceeding it is an error, never
// a pass.
func CheckRegister(ops []Op, budget int) (Result, error) {
	s := newSearch(ops, budget)
	ok, err := s.run(0, 0)
	if err != nil {
		return Result{}, err
	}
	if ok {
		return Result{OK: true}, nil
	}
	return Result{Reason: s.explain()}, nil
}

const never = int64(1) << 62

type search struct {
	ops     []Op
	ret     []int64 // effective return: indeterminate writes never return
	done    []bool
	pending int // definite ops not yet linearized
	seen    map[string]struct{}
	steps   int
	budget  int
	deepest int
}

func newSearch(ops []Op, budget int) *search {
	s := &search{
		ops:    append([]Op(nil), ops...),
		seen:   map[string]struct{}{},
		budget: budget,
	}
	sort.Slice(s.ops, func(i, j int) bool { return s.ops[i].Call < s.ops[j].Call })
	s.ret = make([]int64, len(s.ops))
	s.done = make([]bool, len(s.ops))
	for i, o := range s.ops {
		s.ret[i] = o.Return
		if o.Indeterminate {
			s.ret[i] = never
		} else {
			s.pending++
		}
	}
	return s
}

// run tries to linearize the remaining ops given the register's current
// value; linearized counts ops placed so far (for diagnostics).
func (s *search) run(cur uint64, linearized int) (bool, error) {
	if linearized > s.deepest {
		s.deepest = linearized
	}
	if s.pending == 0 {
		return true, nil // leftover indeterminate writes simply never happened
	}
	if s.steps++; s.steps > s.budget {
		return false, fmt.Errorf("search budget %d exceeded (history too concurrent)", s.budget)
	}
	key := s.memoKey(cur)
	if _, bad := s.seen[key]; bad {
		return false, nil
	}
	horizon := never // earliest return among unlinearized definite ops
	for i := range s.ops {
		if !s.done[i] && s.ret[i] < horizon {
			horizon = s.ret[i]
		}
	}
	for i, o := range s.ops {
		if s.done[i] || o.Call >= horizon {
			continue // o began after some pending op already returned
		}
		next, legal := cur, true
		if o.Kind == Write {
			next = o.Value
		} else {
			legal = o.Value == cur
		}
		if !legal {
			continue
		}
		s.done[i] = true
		if !o.Indeterminate {
			s.pending--
		}
		ok, err := s.run(next, linearized+1)
		s.done[i] = false
		if !o.Indeterminate {
			s.pending++
		}
		if err != nil || ok {
			return ok, err
		}
	}
	s.seen[key] = struct{}{}
	return false, nil
}

func (s *search) memoKey(cur uint64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d|", cur)
	for _, d := range s.done {
		if d {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	return b.String()
}

// explain renders the offending history so a failure is debuggable.
func (s *search) explain() string {
	var b strings.Builder
	fmt.Fprintf(&b, "no linearization (deepest prefix %d of %d ops):", s.deepest, len(s.ops))
	for _, o := range s.ops {
		kind, end := "W", fmt.Sprint(o.Return)
		if o.Kind == Read {
			kind = "R"
		}
		if o.Indeterminate {
			end = "?"
		}
		fmt.Fprintf(&b, "\n  #%d c%d %s v=%d [%d,%s]", o.ID, o.Client, kind, o.Value, o.Call, end)
	}
	return b.String()
}
