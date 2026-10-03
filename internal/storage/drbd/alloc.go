package drbd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/config"
	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

const (
	minorPrefix = "/drbd/minor/"
	portPrefix  = "/drbd/port/"
	volPrefix   = "/drbd/vol/"
	// maxAttempts bounds CAS retries; each retry means another writer made progress.
	maxAttempts = 16
)

// Range is an inclusive span of allocatable numbers.
type Range struct{ Lo, Hi int }

// Default ranges: 1000 volumes per cluster. DefaultPorts is what the firewall opens on the mesh.
var (
	DefaultMinors = Range{Lo: 0, Hi: 999}
	DefaultPorts  = Range{Lo: config.DRBDPortLo, Hi: config.DRBDPortHi}
)

// Allocation is the persisted DRBD identity of one resource: its cluster-wide
// minor and port plus the node-id bookkeeping that keeps rebuilds safe.
type Allocation struct {
	Name  string `json:"name"`
	Minor int    `json:"minor"`
	Port  int    `json:"port"`
	// Next is the count of never-used node-ids handed out (ids 0..Next-1).
	Next    int            `json:"next"`
	NodeIDs map[string]int `json:"nodeIDs"`
	// Diskless maps quorum tiebreakers to their node-ids; they hold no data and are not replicas.
	Diskless map[string]int `json:"diskless,omitempty"`
	// Retired ids belonged to dropped replicas whose slot survivors may still
	// hold; they cannot be reused until ConfirmForgotten.
	Retired []int `json:"retired,omitempty"`
	// Acks lists, per retired id, the members that have forgotten it so far.
	Acks map[int][]string `json:"acks,omitempty"`
	// Forgotten ids were dropped on every survivor and may be recycled once
	// the never-used ids run out.
	Forgotten []int `json:"forgotten,omitempty"`
	// Initialized is set once the volume has served as primary; until then an
	// all-Inconsistent resource may be force-promoted (see volume.HoldOptions).
	Initialized bool `json:"initialized,omitempty"`
}

// Allocator hands out minors, ports and node-ids through the cluster store.
// Every claim is a must-not-exist write, so concurrent callers cannot collide.
type Allocator struct {
	st     store.Store
	minors Range
	ports  Range
}

// NewAllocator returns an allocator drawing from the given ranges.
func NewAllocator(st store.Store, minors, ports Range) *Allocator {
	return &Allocator{st: st, minors: minors, ports: ports}
}

func volKey(name string) store.Key            { return store.Key(volPrefix + name) }
func claimKey(prefix string, n int) store.Key { return store.Key(prefix + strconv.Itoa(n)) }

// Allocate returns the resource's allocation, creating it with the lowest free
// minor and port on first use. Calling it again returns the same numbers.
func (a *Allocator) Allocate(ctx context.Context, name string) (Allocation, error) {
	const op = "drbd.Allocator.Allocate"
	if !resourceName.MatchString(name) {
		return Allocation{}, experrors.New(experrors.KindInvalid, op, fmt.Sprintf("invalid resource name %q", name))
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if al, err := a.Get(ctx, name); err == nil {
			return al, nil
		} else if experrors.KindOf(err) != experrors.KindNotFound {
			return Allocation{}, err
		}
		al, err := a.claim(ctx, name)
		if experrors.KindOf(err) != experrors.KindConflict {
			return al, err
		}
	}
	return Allocation{}, experrors.New(experrors.KindUnavailable, op, "allocation contention did not settle")
}

func (a *Allocator) claim(ctx context.Context, name string) (Allocation, error) {
	const op = "drbd.Allocator.claim"
	minor, err := a.lowestFree(ctx, minorPrefix, a.minors, "minor")
	if err != nil {
		return Allocation{}, err
	}
	port, err := a.lowestFree(ctx, portPrefix, a.ports, "port")
	if err != nil {
		return Allocation{}, err
	}
	al := Allocation{Name: name, Minor: minor, Port: port, NodeIDs: map[string]int{}}
	body, _ := json.Marshal(al)
	keys := []store.Key{claimKey(minorPrefix, minor), claimKey(portPrefix, port), volKey(name)}
	values := [][]byte{[]byte(name), []byte(name), body}
	var ops []store.Op
	for i, k := range keys {
		ops = append(ops, store.Op{Kind: store.OpCheck, Key: k, Expect: 0}, store.Op{Kind: store.OpPut, Key: k, Value: values[i]})
	}
	if _, err := a.st.Txn(ctx, ops); err != nil {
		return Allocation{}, experrors.Wrap(err, experrors.KindOf(err), op, "claim")
	}
	return al, nil
}

func (a *Allocator) lowestFree(ctx context.Context, prefix string, r Range, what string) (int, error) {
	entries, err := a.st.List(ctx, store.Key(prefix))
	if err != nil {
		return 0, experrors.Wrap(err, experrors.KindInternal, "drbd.Allocator.lowestFree", "list "+what+" claims")
	}
	taken := make(map[int]bool, len(entries))
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.TrimPrefix(string(e.Key), prefix)); err == nil {
			taken[n] = true
		}
	}
	for n := r.Lo; n <= r.Hi; n++ {
		if !taken[n] {
			return n, nil
		}
	}
	return 0, experrors.New(experrors.KindResourceExhausted, "drbd.Allocator.lowestFree", fmt.Sprintf("no free %s in %d..%d", what, r.Lo, r.Hi))
}

// Get returns the persisted allocation, or KindNotFound.
func (a *Allocator) Get(ctx context.Context, name string) (Allocation, error) {
	al, _, err := a.load(ctx, name)
	return al, err
}

func (a *Allocator) load(ctx context.Context, name string) (Allocation, store.Revision, error) {
	e, err := a.st.Get(ctx, volKey(name))
	if err != nil {
		return Allocation{}, 0, experrors.Wrap(err, experrors.KindOf(err), "drbd.Allocator.load", fmt.Sprintf("resource %q", name))
	}
	var al Allocation
	if err := json.Unmarshal(e.Value, &al); err != nil {
		return Allocation{}, 0, experrors.Wrap(err, experrors.KindInternal, "drbd.Allocator.load", fmt.Sprintf("resource %q: corrupt record", name))
	}
	if al.NodeIDs == nil {
		al.NodeIDs = map[string]int{}
	}
	if al.Diskless == nil {
		al.Diskless = map[string]int{}
	}
	return al, e.Revision, nil
}

// Release frees the resource's minor, port and record.
func (a *Allocator) Release(ctx context.Context, name string) error {
	al, rev, err := a.load(ctx, name)
	if err != nil {
		return err
	}
	_, err = a.st.Txn(ctx, []store.Op{
		{Kind: store.OpDelete, Key: volKey(name), Expect: rev},
		{Kind: store.OpDelete, Key: claimKey(minorPrefix, al.Minor)},
		{Kind: store.OpDelete, Key: claimKey(portPrefix, al.Port)},
	})
	return wrapTxn(err, "drbd.Allocator.Release")
}

// AssignNodeID returns the host's node-id, giving it a fresh one on first call.
func (a *Allocator) AssignNodeID(ctx context.Context, name, host string) (int, error) {
	var id int
	err := a.update(ctx, name, func(al *Allocation) (err error) { id, err = al.assign(host); return })
	return id, err
}

// AssignDiskless returns the host's tiebreaker node-id, giving it a fresh one on first call.
func (a *Allocator) AssignDiskless(ctx context.Context, name, host string) (int, error) {
	var id int
	err := a.update(ctx, name, func(al *Allocation) (err error) { id, err = al.assignDiskless(host); return })
	return id, err
}

// RetireNode drops the host's replica or tiebreaker and returns its node-id, which stays
// unusable until ConfirmForgotten. Call it once the controller has declared
// the replica lost, before running forget-peer on the survivors.
func (a *Allocator) RetireNode(ctx context.Context, name, host string) (int, error) {
	var id int
	err := a.update(ctx, name, func(al *Allocation) (err error) { id, err = al.retire(host); return })
	return id, err
}

// ConfirmForgotten records that forget-peer succeeded on every survivor for id.
func (a *Allocator) ConfirmForgotten(ctx context.Context, name string, id int) error {
	return a.update(ctx, name, func(al *Allocation) error { return al.confirmForgotten(id) })
}

// AckForgotten records that host ran forget-peer for a retired id. Once every
// current member has, the id becomes reusable. It is safe to repeat.
func (a *Allocator) AckForgotten(ctx context.Context, name string, id int, host string) error {
	return a.update(ctx, name, func(al *Allocation) error { return al.ack(id, host) })
}

// MarkInitialized records that the volume has served as primary. It is safe to repeat.
func (a *Allocator) MarkInitialized(ctx context.Context, name string) error {
	return a.update(ctx, name, func(al *Allocation) error { al.Initialized = true; return nil })
}

// update applies fn to the record under compare-and-swap, retrying on races.
func (a *Allocator) update(ctx context.Context, name string, fn func(*Allocation) error) error {
	const op = "drbd.Allocator.update"
	for attempt := 0; attempt < maxAttempts; attempt++ {
		al, rev, err := a.load(ctx, name)
		if err != nil {
			return err
		}
		if err := fn(&al); err != nil {
			return err
		}
		body, _ := json.Marshal(al)
		_, err = a.st.CompareAndSwap(ctx, volKey(name), rev, body)
		if experrors.KindOf(err) != experrors.KindConflict {
			return wrapTxn(err, op)
		}
	}
	return experrors.New(experrors.KindUnavailable, op, "update contention did not settle")
}

func wrapTxn(err error, op string) error {
	if err == nil {
		return nil
	}
	return experrors.Wrap(err, experrors.KindOf(err), op, "store")
}

func (al *Allocation) assign(host string) (int, error) {
	if id, ok := al.NodeIDs[host]; ok {
		return id, nil
	}
	if _, ok := al.Diskless[host]; ok {
		return 0, al.roleConflict("drbd.Allocation.assign", host, "a tiebreaker")
	}
	id, err := al.nextID()
	if err != nil {
		return 0, err
	}
	al.NodeIDs[host] = id
	return id, nil
}

func (al *Allocation) assignDiskless(host string) (int, error) {
	if id, ok := al.Diskless[host]; ok {
		return id, nil
	}
	if _, ok := al.NodeIDs[host]; ok {
		return 0, al.roleConflict("drbd.Allocation.assignDiskless", host, "a replica")
	}
	id, err := al.nextID()
	if err != nil {
		return 0, err
	}
	al.Diskless[host] = id
	return id, nil
}

func (al *Allocation) roleConflict(op, host, role string) error {
	return experrors.New(experrors.KindConflict, op, fmt.Sprintf("resource %q: host %q is already %s", al.Name, host, role))
}

// nextID prefers a never-used id; only when all 32 are spent does it recycle
// the lowest id every survivor has confirmed forgotten.
func (al *Allocation) nextID() (int, error) {
	if al.Next <= maxNodeID {
		al.Next++
		return al.Next - 1, nil
	}
	if len(al.Forgotten) > 0 {
		id := al.Forgotten[0]
		al.Forgotten = al.Forgotten[1:]
		return id, nil
	}
	return 0, experrors.New(experrors.KindResourceExhausted, "drbd.Allocation.assign",
		fmt.Sprintf("resource %q: all %d node-ids are live or awaiting forget-peer", al.Name, maxNodeID+1))
}

func (al *Allocation) retire(host string) (int, error) {
	id, ok := al.NodeIDs[host]
	delete(al.NodeIDs, host)
	if !ok {
		id, ok = al.Diskless[host]
		delete(al.Diskless, host)
	}
	if !ok {
		return 0, experrors.New(experrors.KindNotFound, "drbd.Allocation.retire", fmt.Sprintf("resource %q: host %q has no node-id", al.Name, host))
	}
	al.Retired = insertSorted(al.Retired, id)
	return id, nil
}

func (al *Allocation) ack(id int, host string) error {
	if slices.Contains(al.Forgotten, id) {
		return nil
	}
	if !slices.Contains(al.Retired, id) {
		return experrors.New(experrors.KindNotFound, "drbd.Allocation.ack", fmt.Sprintf("resource %q: node-id %d is not retired", al.Name, id))
	}
	if _, member := al.NodeIDs[host]; !member {
		return experrors.New(experrors.KindInvalid, "drbd.Allocation.ack", fmt.Sprintf("resource %q: %q is not a member", al.Name, host))
	}
	if al.Acks == nil {
		al.Acks = map[int][]string{}
	}
	if !slices.Contains(al.Acks[id], host) {
		al.Acks[id] = append(al.Acks[id], host)
	}
	for member := range al.NodeIDs {
		if !slices.Contains(al.Acks[id], member) {
			return nil
		}
	}
	return al.confirmForgotten(id)
}

func (al *Allocation) confirmForgotten(id int) error {
	i := sort.SearchInts(al.Retired, id)
	if i == len(al.Retired) || al.Retired[i] != id {
		return experrors.New(experrors.KindNotFound, "drbd.Allocation.confirmForgotten", fmt.Sprintf("resource %q: node-id %d is not retired", al.Name, id))
	}
	al.Retired = append(al.Retired[:i:i], al.Retired[i+1:]...)
	delete(al.Acks, id)
	al.Forgotten = insertSorted(al.Forgotten, id)
	return nil
}

func insertSorted(s []int, v int) []int {
	s = append(s, v)
	sort.Ints(s)
	return s
}
