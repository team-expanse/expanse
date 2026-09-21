// Package linearizability is a Jepsen-style history checker for block volumes.
// Every 4 KiB block is a read/write register; clients record (call, return, op,
// result) histories and CheckHistory decides, exactly and per block, whether
// some total order consistent with real time explains every observed read. The
// search is Porcupine's (github.com/anishathalye/porcupine), not ours. A write
// whose outcome is unknown is "indeterminate" and may or may not have taken
// effect. Any acked-write loss is a release blocker.
package linearizability

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/anishathalye/porcupine"
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
// timeout bounds the search per block; an unfinished search is an error,
// never a pass.
func CheckHistory(ops []Op, timeout time.Duration) (Result, error) {
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
		res, err := CheckRegister(byKey[k], timeout)
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
// value is 0. An indeterminate write is modelled as one that never
// returned: it may take effect at any time after its call, and linearizing
// it last is always available, so "it never happened" stays legal.
func CheckRegister(ops []Op, timeout time.Duration) (Result, error) {
	return checkRegister(withoutUnobservedWrites(ops), timeout)
}

// withoutUnobservedWrites drops indeterminate writes whose value no read
// returned. Such a write can always be linearized last, after every read, so
// it can never explain or contradict one — and left in, every ordering of the
// pending writes is searched.
func withoutUnobservedWrites(ops []Op) []Op {
	seen := map[uint64]bool{}
	for _, o := range ops {
		if o.Kind == Read {
			seen[o.Value] = true
		}
	}
	kept := make([]Op, 0, len(ops))
	for _, o := range ops {
		if o.Indeterminate && o.Kind == Write && !seen[o.Value] {
			continue
		}
		kept = append(kept, o)
	}
	return kept
}

func checkRegister(ops []Op, timeout time.Duration) (Result, error) {
	hist := make([]porcupine.Operation, len(ops))
	for i, o := range ops {
		in := registerInput{write: o.Kind == Write, value: o.Value}
		ret := o.Return
		if o.Indeterminate {
			ret = math.MaxInt64
		}
		hist[i] = porcupine.Operation{ClientId: o.Client, Input: in, Call: o.Call, Output: o.Value, Return: ret}
	}
	switch porcupine.CheckOperationsTimeout(registerModel, hist, timeout) {
	case porcupine.Ok:
		return Result{OK: true}, nil
	case porcupine.Illegal:
		return Result{Reason: explain(ops)}, nil
	default:
		return Result{}, fmt.Errorf("search did not finish within %v (%d ops)", timeout, len(ops))
	}
}

type registerInput struct {
	write bool
	value uint64
}

// registerModel is a single read/write register starting at 0.
var registerModel = porcupine.Model{
	Init: func() interface{} { return uint64(0) },
	Step: func(state, input, output interface{}) (bool, interface{}) {
		in := input.(registerInput)
		if in.write {
			return true, in.value
		}
		return output.(uint64) == state.(uint64), state
	},
}

// explain renders the offending history so a failure is debuggable.
func explain(ops []Op) string {
	sorted := append([]Op(nil), ops...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Call < sorted[j].Call })
	var b strings.Builder
	fmt.Fprintf(&b, "no linearization of %d ops:", len(sorted))
	for _, o := range sorted {
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
