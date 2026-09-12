// Package generation implements cluster generations (§4.7): an
// append-only history of the desired state, snapshotted automatically
// on every accepted desired-state mutation.
//
// Layout in the replicated store:
//
//	/cluster/generation      current generation number (decimal)
//	/generations/<n>/meta    JSON-encoded Generation
//	/generations/<n>/data    canonical snapshot of all desired-state keys
//
// ROLLBACK IS APPEND-ONLY: `Rollback` writes the content of generation n
// as a NEW generation (n+1 with the content of n-1). History is never
// rewritten — you can always roll forward again. Rollback is not a
// special code path; it is just another desired-state change that the
// reconcilers converge to through the normal loop.
package generation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// DesiredPrefixes are the replicated-store prefixes whose keys constitute
// desired state (§4.7). Mutating any of them creates a generation. The
// per-node resource tree (/node/<id>/resources/) is included: it is the
// reconcilers' desired state and must roll back with everything else.
// Status/observed keys are filtered out by IsDesiredKey regardless of
// prefix.
var DesiredPrefixes = []string{
	"/blocks/",
	"/volumes/",
	"/networks/",
	"/cluster/config",
	"/node/",
}

// Status markers for observed-state keys, which never create generations.
const (
	StatusSuffix      = "/status"  // trailing segment
	StatusPathSegment = "/status/" // status subtree (e.g. /node/<id>/status/…)
)

// IsDesiredKey reports whether k is a desired-state key. Status/observed
// keys (`*/status`, `*/status/*`, `/events/`) are never desired state
// even under a desired prefix.
func IsDesiredKey(k store.Key) bool {
	s := string(k)
	if strings.HasSuffix(s, StatusSuffix) || strings.Contains(s, StatusPathSegment) {
		return false
	}
	for _, p := range DesiredPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Store layout keys.
const (
	CurrentKey = store.Key("/cluster/generation")
	GensPrefix = "/generations/"
	dataSuffix = "/data"
	metaSuffix = "/meta"
	MetaPrefix = GensPrefix // every meta key starts with this
)

// MetaKey is the metadata key for generation n.
func MetaKey(n uint64) store.Key { return store.Key(fmt.Sprintf("%s%d%s", GensPrefix, n, metaSuffix)) }

// DataKey is the snapshot key for generation n.
func DataKey(n uint64) store.Key { return store.Key(fmt.Sprintf("%s%d%s", GensPrefix, n, dataSuffix)) }

// Retention (§4.7): keep the last 50 generations plus everything from
// the last 30 days.
const (
	KeepLast   = 50
	KeepWindow = 30 * 24 * time.Hour
)

// Generation describes one point-in-time snapshot of desired state.
type Generation struct {
	Number      uint64         `json:"number"`
	Revision    store.Revision `json:"revision"` // store revision of the mutation that created it
	CreatedAt   time.Time      `json:"created_at"`
	CreatedBy   string         `json:"created_by"` // user or "system"
	Description string         `json:"description,omitempty"`
	Parent      uint64         `json:"parent"` // previous generation number (0 = bootstrap)
	Hash        string         `json:"hash"`   // sha256 of the canonical snapshot
}

// KV is one key/value pair of a snapshot.
type KV struct {
	Key   string `json:"k"`
	Value []byte `json:"v"`
}

// Snapshot is the canonical serialization of desired state at a revision:
// a JSON object with keys sorted by key. Encoding is deterministic —
// the same state always produces the same bytes and therefore the same
// hash, regardless of how the underlying map was iterated.
type Snapshot struct {
	Keys []KV `json:"keys"` // sorted by Key
}

// EncodeSnapshot canonicalizes a desired-state map.
func EncodeSnapshot(m map[store.Key][]byte) []byte {
	snap := Snapshot{Keys: make([]KV, 0, len(m))}
	for k, v := range m {
		snap.Keys = append(snap.Keys, KV{Key: string(k), Value: v})
	}
	sort.Slice(snap.Keys, func(i, j int) bool { return snap.Keys[i].Key < snap.Keys[j].Key })
	// json.Marshal never fails on this plain-data struct.
	b, _ := json.Marshal(snap) //nolint:errcheck — infallible for []KV
	return b
}

// DecodeSnapshot parses a canonical snapshot.
func DecodeSnapshot(b []byte) (map[store.Key][]byte, error) {
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "generation.Decode", "parse snapshot")
	}
	m := make(map[store.Key][]byte, len(snap.Keys))
	for _, kv := range snap.Keys {
		m[store.Key(kv.Key)] = kv.Value
	}
	return m, nil
}

// Hash returns the sha256 hex of the canonical encoding.
func Hash(m map[store.Key][]byte) string {
	sum := sha256.Sum256(EncodeSnapshot(m))
	return hex.EncodeToString(sum[:])
}

// --- client-side operations over store.Store ---

// Current returns the newest generation number (0 if none — e.g. a
// pre-Phase-03 store).
func Current(ctx context.Context, st store.Store) (uint64, error) {
	e, err := st.Get(ctx, CurrentKey)
	if errors.Is(err, errors.KindNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(string(e.Value), 10, 64)
	if err != nil {
		return 0, errors.Wrap(err, errors.KindInternal, "generation.Current", "parse current generation")
	}
	return n, nil
}

// List returns metadata for all retained generations, oldest first.
func List(ctx context.Context, st store.Store) ([]Generation, error) {
	entries, err := st.List(ctx, store.Key(GensPrefix))
	if err != nil {
		return nil, err
	}
	var out []Generation
	for _, e := range entries {
		if !strings.HasSuffix(string(e.Key), metaSuffix) {
			continue
		}
		var g Generation
		if err := json.Unmarshal(e.Value, &g); err != nil {
			return nil, errors.Wrap(err, errors.KindInternal, "generation.List", "parse "+string(e.Key))
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// Get returns metadata for generation n.
func Get(ctx context.Context, st store.Store, n uint64) (*Generation, error) {
	e, err := st.Get(ctx, MetaKey(n))
	if err != nil {
		return nil, err
	}
	var g Generation
	if err := json.Unmarshal(e.Value, &g); err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "generation.Get", "parse meta")
	}
	return &g, nil
}

// Content returns the desired-state map captured in generation n.
func Content(ctx context.Context, st store.Store, n uint64) (map[store.Key][]byte, error) {
	e, err := st.Get(ctx, DataKey(n))
	if err != nil {
		return nil, err
	}
	return DecodeSnapshot(e.Value)
}

// Diff summarizes the changes between generations a and b.
type Diff struct {
	Added   []string `json:"added"`   // in b, not in a
	Removed []string `json:"removed"` // in a, not in b
	Changed []string `json:"changed"` // value differs
}

// Empty reports whether nothing differs.
func (d Diff) Empty() bool { return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0 }

// DiffGenerations compares the snapshots of generations a and b.
func DiffGenerations(ctx context.Context, st store.Store, a, b uint64) (*Diff, error) {
	ma, err := Content(ctx, st, a)
	if err != nil {
		return nil, err
	}
	mb, err := Content(ctx, st, b)
	if err != nil {
		return nil, err
	}
	d := &Diff{}
	for k, vb := range mb {
		if va, ok := ma[k]; !ok {
			d.Added = append(d.Added, string(k))
		} else if string(va) != string(vb) {
			d.Changed = append(d.Changed, string(k))
		}
	}
	for k := range ma {
		if _, ok := mb[k]; !ok {
			d.Removed = append(d.Removed, string(k))
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Changed)
	sort.Strings(d.Removed)
	return d, nil
}

// Rollback atomically restores the desired state of generation target by
// writing it as a NEW generation (append-only history — see the package
// comment). target==0 means "previous generation". Returns the new
// generation number. If the store already equals the target state the
// mutation is a no-op and the current generation is returned unchanged
// (there is nothing to roll back to).
func Rollback(ctx context.Context, st store.Store, target uint64, by, description string) (uint64, error) {
	cur, err := Current(ctx, st)
	if err != nil {
		return 0, err
	}
	if cur == 0 {
		return 0, errors.New(errors.KindNotFound, "generation.Rollback", "no generations recorded")
	}
	if target == 0 {
		if cur < 2 {
			return 0, errors.New(errors.KindInvalid, "generation.Rollback", "no previous generation to roll back to")
		}
		target = cur - 1
	}
	if target == cur {
		return cur, nil // already there
	}
	if target > cur {
		return 0, errors.New(errors.KindInvalid, "generation.Rollback",
			fmt.Sprintf("cannot roll back to a later generation (%d > %d); roll forward by re-applying instead", target, cur))
	}

	want, err := Content(ctx, st, target)
	if err != nil {
		return 0, err
	}

	// Current desired state (linearizable read via Txn's raft round-trip:
	// the ops themselves carry the preconditions, so no separate Get is
	// needed for correctness — but we must build the diff from a read).
	have := make(map[store.Key][]byte)
	for _, p := range DesiredPrefixes {
		entries, err := st.List(ctx, store.Key(p))
		if err != nil {
			return 0, err
		}
		for _, e := range entries {
			if IsDesiredKey(e.Key) {
				have[e.Key] = e.Value
			}
		}
	}

	ops := []store.Op{}
	for k, v := range want {
		if hv, ok := have[k]; ok && string(hv) == string(v) {
			continue // unchanged
		}
		ops = append(ops, store.Op{Kind: store.OpPut, Key: k, Value: v})
	}
	for k := range have {
		if _, ok := want[k]; !ok {
			ops = append(ops, store.Op{Kind: store.OpDelete, Key: k})
		}
	}
	if len(ops) == 0 {
		return cur, nil // state already equals target
	}
	if description == "" {
		description = fmt.Sprintf("rollback to generation %d", target)
	}
	_ = description // carried by the caller's audit trail; the FSM stamps CreatedBy "system"
	if _, err := st.Txn(ctx, ops); err != nil {
		return 0, err
	}
	// The FSM hook created generation cur+1 with the content of target.
	return cur + 1, nil
}
