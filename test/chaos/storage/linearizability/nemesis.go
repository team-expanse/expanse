package linearizability

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/test/chaos/exvol"
)

// Nemesis injects one fault at a time (never more than a minority is
// affected, so quorum survives and every acked write must too): kill the
// primary, partition it or a secondary, SIGSTOP-pause either, or slow a
// node's links. After a primary is knocked out it plays the controller
// and elects a replacement.
type Nemesis struct {
	C     *exvol.Cluster
	Vol   string
	Nodes []string

	rng    *rand.Rand
	mu     sync.Mutex
	counts map[string]int
	stuck  string // why injection stopped early (cluster never re-healed)
	logf   func(string, ...any)
}

const (
	healthyBudget = 45 * time.Second // per-fault recovery allowance
	healthyLag    = 32               // ops a live secondary may trail the primary
)

func NewNemesis(c *exvol.Cluster, vol string, nodes []string, logf func(string, ...any)) *Nemesis {
	return &Nemesis{
		C: c, Vol: vol, Nodes: nodes, logf: logf,
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())),
		counts: map[string]int{},
	}
}

// Stuck is non-empty if the nemesis stopped because the cluster never
// recovered from a fault within healthyBudget.
func (n *Nemesis) Stuck() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stuck
}

// Counts reports how many times each fault fired.
func (n *Nemesis) Counts() map[string]int {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := map[string]int{}
	for k, v := range n.counts {
		out[k] = v
	}
	return out
}

// Run injects faults until stop closes, healing before it returns.
func (n *Nemesis) Run(stop <-chan struct{}) {
	faults := []struct {
		name           string
		needsSuccessor bool // knocks out the primary, so a current replica must exist to take over
		fn             func(stop <-chan struct{})
	}{
		{"kill-primary", true, n.killPrimary},
		{"isolate-primary", true, func(s <-chan struct{}) { n.hold("isolate-primary", true, n.C.Faults.Isolate, n.C.Faults.Heal, s) }},
		{"isolate-secondary", false, func(s <-chan struct{}) { n.hold("isolate-secondary", false, n.C.Faults.Isolate, n.C.Faults.Heal, s) }},
		{"pause-primary", true, func(s <-chan struct{}) { n.hold("pause-primary", true, n.C.Faults.Pause, n.C.Faults.Resume, s) }},
		{"pause-secondary", false, func(s <-chan struct{}) { n.hold("pause-secondary", false, n.C.Faults.Pause, n.C.Faults.Resume, s) }},
		{"slow-node", false, n.slowNode},
	}
	for {
		if !n.sleep(stop, 500*time.Millisecond, 1500*time.Millisecond) {
			return
		}
		t0 := time.Now()
		why := n.waitHealthy(stop)
		if d := time.Since(t0); d > time.Second {
			n.logf("nemesis: waited %v for the cluster to re-heal", d.Round(time.Millisecond))
		}
		if why != "" {
			n.mu.Lock()
			n.stuck = why
			n.mu.Unlock()
			n.logf("nemesis: cluster never re-healed, no more faults: %s", why)
			return
		}
		f := faults[n.rng.Intn(len(faults))]
		if f.needsSuccessor && len(n.electable(n.primary())) == 0 {
			continue // e.g. the only other live replica is still resyncing (Stale)
		}
		n.mu.Lock()
		n.counts[f.name]++
		n.mu.Unlock()
		n.logf("nemesis: %s", f.name)
		f.fn(stop)
	}
}

// waitHealthy blocks until every node is up, no replica is Stale or
// Resyncing, and every secondary is within healthyLag ops of the primary
// (faults are injected one at a time, recovery included). It returns ""
// when healthy or stop closed, else why the cluster stayed unhealthy.
func (n *Nemesis) waitHealthy(stop <-chan struct{}) string {
	deadline := time.Now().Add(healthyBudget)
	why := n.unhealthy()
	for why != "" && time.Now().Before(deadline) {
		select {
		case <-stop:
			return ""
		case <-time.After(250 * time.Millisecond):
		}
		why = n.unhealthy()
	}
	return why
}

func (n *Nemesis) unhealthy() string {
	st := n.C.Status(n.Vol)
	for _, pl := range st.Placement {
		if pl.Role == storage.RoleStale || pl.Role == storage.RoleResyncing {
			return fmt.Sprintf("%s is %s", pl.NodeID, pl.Role)
		}
	}
	seqs := map[string]uint64{}
	var top uint64
	for _, id := range n.Nodes {
		if !n.C.Node(id).Alive() {
			return id + " is down"
		}
		seq, err := n.C.ProbeSeq(n.Vol, id)
		if err != nil {
			return fmt.Sprintf("%s does not answer probes: %v", id, err)
		}
		seqs[id] = seq
		if seq > top {
			top = seq
		}
	}
	for id, seq := range seqs {
		if top-seq > healthyLag {
			return fmt.Sprintf("%s is at seq %d, %d behind %d", id, seq, top-seq, top)
		}
	}
	return ""
}

// sleep waits a random duration in [lo,hi]; false means stop was closed.
func (n *Nemesis) sleep(stop <-chan struct{}, lo, hi time.Duration) bool {
	d := lo + time.Duration(n.rng.Int63n(int64(hi-lo)+1))
	select {
	case <-stop:
		return false
	case <-time.After(d):
		return true
	}
}

func (n *Nemesis) primary() string { return n.C.Status(n.Vol).Primary }

// other picks a live node that is not `not`.
func (n *Nemesis) other(not string) string {
	var cands []string
	for _, id := range n.Nodes {
		if id != not && n.C.Node(id).Alive() {
			cands = append(cands, id)
		}
	}
	return cands[n.rng.Intn(len(cands))]
}

// electable lists what the controller may elect: live nodes other than
// `not` whose persisted role is not Stale (a Stale copy is mid-resync or
// an uncommitted branch, never a primary candidate).
func (n *Nemesis) electable(not string) []string {
	stale := map[string]bool{}
	for _, pl := range n.C.Status(n.Vol).Placement {
		stale[pl.NodeID] = pl.Role == storage.RoleStale
	}
	var out []string
	for _, id := range n.Nodes {
		if id != not && n.C.Node(id).Alive() && !stale[id] {
			out = append(out, id)
		}
	}
	return out
}

// bestCandidate mirrors the controller's currency gate (electPrimary): of
// the electable nodes that answer a live probe, the highest sequence wins,
// ties to the lowest ID. ok is false when none answers.
func (n *Nemesis) bestCandidate(not string) (id string, ok bool) {
	var best uint64
	for _, cand := range n.electable(not) { // Nodes order: lowest ID first
		seq, err := n.C.ProbeSeq(n.Vol, cand)
		if err != nil {
			continue
		}
		if !ok || seq > best {
			id, best, ok = cand, seq, true
		}
	}
	return id, ok
}

// failover is the controller's job in production: elect a new primary.
func (n *Nemesis) failover(old string) {
	next, ok := n.bestCandidate(old)
	if !ok {
		n.logf("nemesis: no electable successor for %s; leaving it elected", old)
		return
	}
	n.logf("nemesis: elect %s (was %s)", next, old)
	n.C.Elect(n.Vol, next)
}

func (n *Nemesis) killPrimary(stop <-chan struct{}) {
	p := n.primary()
	n.C.Crash(p)
	n.failover(p)
	n.sleep(stop, time.Second, 3*time.Second)
	n.C.Restart(p)
}

// hold applies a node fault to the primary or a secondary for several
// seconds (longer than the 2 s lease TTL, so a fenced primary really
// loses its lease), electing a successor when the primary is the victim.
func (n *Nemesis) hold(name string, victimIsPrimary bool, apply, undo func(int), stop <-chan struct{}) {
	victim := n.primary()
	if !victimIsPrimary {
		victim = n.other(victim)
	}
	idx := n.C.Node(victim).Idx
	apply(idx)
	if victimIsPrimary {
		n.failover(victim)
	}
	n.sleep(stop, 2500*time.Millisecond, 4*time.Second)
	undo(idx)
	n.logf("nemesis: %s on %s healed", name, victim)
}

func (n *Nemesis) slowNode(stop <-chan struct{}) {
	victim := n.other("")
	idx := n.C.Node(victim).Idx
	n.C.Faults.SetDelay(idx, 20*time.Millisecond)
	n.sleep(stop, 2*time.Second, 4*time.Second)
	n.C.Faults.SetDelay(idx, 0)
}

// Settle heals every fault and brings every node back up.
func (n *Nemesis) Settle() error {
	n.C.Faults.HealAll()
	for _, id := range n.Nodes {
		n.C.Restart(id)
	}
	if !n.C.Node(n.primary()).Alive() {
		return fmt.Errorf("status primary %s is not alive after settle", n.primary())
	}
	return nil
}
