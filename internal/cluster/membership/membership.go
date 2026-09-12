// Package membership provides cluster membership gossip over
// github.com/hashicorp/memberlist (spec §4.2). It maintains a live view of
// which nodes exist and whether they respond, plus each node's small
// gossiped metadata blob.
//
// Relationship between memberlist and Raft — this is the load-bearing
// design rule of this package:
//
//   - memberlist is for LIVENESS (fast, eventually consistent, may be wrong)
//   - Raft is for TRUTH (slow, strongly consistent, always right)
//   - memberlist failure detection TRIGGERS action; Raft AUTHORIZES it
//   - Never evict a node from Raft based on memberlist alone without a
//     quorum decision (spec §10 "Memberlist false positives")
//
// Concretely: a node that memberlist marks dead has its gossiped state set
// to unreachable here — nothing more. Demotion to non-voter, removal from
// the Raft configuration, or any other destructive change must go through
// a Raft-logged, quorum-replicated decision made by higher layers.
package membership

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"

	"github.com/expanse/expanse/internal/errors"
)

// Role is a node's role in the cluster, gossiped as metadata.
type Role string

const (
	RoleVoter    Role = "voter"    // participates in Raft quorum
	RoleNonVoter Role = "nonvoter" // replicates but does not vote
	RoleWitness  Role = "witness"  // votes but holds no data (spec §4.9)
)

// NodeMeta is the per-node metadata gossiped through memberlist. Keep it
// small: memberlist caps node metadata at 512 bytes (we enforce that at
// marshal time), and every byte is re-transmitted on every alive message.
type NodeMeta struct {
	NodeID       string   `json:"id"`
	Role         Role     `json:"role"`
	Version      string   `json:"ver,omitempty"`
	RaftAddr     string   `json:"raft,omitempty"`
	APIAddr      string   `json:"api,omitempty"`
	Capabilities []string `json:"cap,omitempty"`
}

// Marshal serializes the metadata for gossip. The "truncated set" rule:
// if capabilities push the blob over the limit they are dropped wholesale
// rather than corrupting the rest of the metadata.
func (m NodeMeta) Marshal() ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "membership.NodeMeta.Marshal", err.Error())
	}
	if len(b) > memberlistMetaLimit {
		trimmed := m
		trimmed.Capabilities = nil
		b, err = json.Marshal(trimmed)
		if err != nil {
			return nil, errors.New(errors.KindInternal, "membership.NodeMeta.Marshal", err.Error())
		}
	}
	if len(b) > memberlistMetaLimit {
		return nil, errors.New(errors.KindInvalid, "membership.NodeMeta.Marshal",
			fmt.Sprintf("node metadata is %d bytes, limit is %d", len(b), memberlistMetaLimit))
	}
	return b, nil
}

// unmarshalNodeMeta parses gossiped metadata; garbage is reported as an
// empty meta rather than an error so one bad node cannot log-spam peers.
func unmarshalNodeMeta(b []byte) NodeMeta {
	var m NodeMeta
	_ = json.Unmarshal(b, &m)
	return m
}

// memberlistMetaLimit mirrors memberlist's internal 512-byte metadata cap
// (consul: "metaMaxSize").
const memberlistMetaLimit = 512

// EventKind classifies a membership change observed through gossip.
type EventKind int

const (
	EventJoin   EventKind = iota // node first seen alive
	EventUpdate                  // node metadata changed
	EventLeave                   // node left or was confirmed dead
)

func (k EventKind) String() string {
	switch k {
	case EventJoin:
		return "join"
	case EventUpdate:
		return "update"
	case EventLeave:
		return "leave"
	}
	return "unknown"
}

// Event is a membership change. Node carries the parsed metadata; note
// that on EventLeave the metadata may be zero if the node never gossiped
// any (e.g. detected by address only).
type Event struct {
	Kind EventKind
	Node NodeMeta
}

// NodeState is the local liveness view of one node. State mirrors
// memberlist's own alive/suspect/dead classification.
type NodeState struct {
	Meta  NodeMeta
	Addr  string
	State string // "alive" | "suspect" | "dead" | "left"
	Since time.Time
}

// Config configures an Agent.
type Config struct {
	NodeID   string
	BindAddr string // IP to bind (default 0.0.0.0)
	BindPort int    // TCP+UDP port, fixed at 7445 (config.PortMemberlist)

	// ClusterSecret is the shared cluster secret; the memberlist
	// encryption key is derived from it (memberlist encryption is its
	// own, independent of the mTLS used by Raft and the API).
	ClusterSecret []byte

	Meta NodeMeta // this node's gossiped metadata

	// OnEvent is invoked (synchronously, from memberlist's handoff
	// goroutine) for every membership change. Implementations must be
	// fast and non-blocking; the ONLY thing they should do is update
	// node state. Never perform Raft mutations from here.
	OnEvent func(Event)
}

// Agent is one node's memberlist instance plus the local membership view.
type Agent struct {
	ml      *memberlist.Memberlist
	conf    Config
	local   NodeMeta
	localMu sync.RWMutex

	mu     sync.Mutex
	states map[string]*NodeState // by node ID
}

// DeriveKey maps the cluster secret to the 32-byte key memberlist needs
// for AES-256 gossip encryption. Deriving (rather than using the secret
// raw) keeps the option of rotating the gossip key separately from the
// cluster secret, and pins the key length regardless of secret length.
func DeriveKey(clusterSecret []byte) []byte {
	k := sha256.Sum256(clusterSecret)
	return k[:]
}

// NewAgent creates the node's memberlist instance (listening, but not yet
// joined to anyone).
func NewAgent(conf Config) (*Agent, error) {
	if conf.ClusterSecret == nil {
		return nil, errors.New(errors.KindInvalid, "membership.NewAgent", "cluster secret is required")
	}
	if conf.Meta.NodeID == "" {
		return nil, errors.New(errors.KindInvalid, "membership.NewAgent", "node ID is required")
	}
	if conf.Meta.Role == "" {
		return nil, errors.New(errors.KindInvalid, "membership.NewAgent", "node role is required")
	}

	if _, err := conf.Meta.Marshal(); err != nil {
		return nil, err // reject over-limit metadata up front
	}

	a := &Agent{
		conf:   conf,
		local:  conf.Meta,
		states: make(map[string]*NodeState),
	}

	c := memberlist.DefaultLANConfig()
	c.Name = conf.Meta.NodeID
	if conf.BindAddr != "" {
		c.BindAddr = conf.BindAddr
	}
	c.BindPort = conf.BindPort
	c.AdvertisePort = conf.BindPort

	// Spec §4.2 tuning (ProbeTimeout, ProbeInterval, GossipInterval are
	// already the LAN defaults; SuspicionMult is 4 by default and is the
	// reason we override): with ProbeInterval 1s and SuspicionMult 3 a
	// silent node is confirmed dead in ~3 * log(N+1) * 1s — ~3s at
	// 3 nodes.
	c.ProbeInterval = 1 * time.Second
	c.ProbeTimeout = 500 * time.Millisecond
	c.SuspicionMult = 3
	c.GossipInterval = 200 * time.Millisecond

	// Encryption: 32-byte AES-GCM key derived from the cluster secret.
	// Verification is on for both gossip and piggybacked state.
	key := DeriveKey(conf.ClusterSecret)
	c.SecretKey = key
	c.GossipVerifyIncoming = true
	c.GossipVerifyOutgoing = true

	c.LogOutput = nil // memberlist logs via the standard logger; daemon wiring silences or redirects it

	d := &agentDelegate{agent: a}
	c.Delegate = d
	c.Events = d

	ml, err := memberlist.Create(c)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "membership.NewAgent", "memberlist create: "+err.Error())
	}
	a.ml = ml
	return a, nil
}

// Join connects the node to an existing cluster. Returns the number of
// nodes successfully contacted.
func (a *Agent) Join(existing []string) (int, error) {
	n, err := a.ml.Join(existing)
	if err != nil {
		return n, errors.New(errors.KindUnavailable, "membership.Join", "join failed: "+err.Error())
	}
	return n, nil
}

// Members returns the current local view of all nodes, including this one.
func (a *Agent) Members() []NodeState {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]NodeState, 0, len(a.states)+1)
	for _, s := range a.states {
		out = append(out, *s)
	}
	return out
}

// State returns the local view of one node (zero value if unknown).
func (a *Agent) State(nodeID string) (NodeState, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.states[nodeID]
	if !ok {
		return NodeState{}, false
	}
	return *s, true
}

// UpdateMeta changes this node's gossiped metadata (e.g. role demotion)
// and broadcasts the update.
func (a *Agent) UpdateMeta(m NodeMeta) error {
	a.localMu.Lock()
	a.local = m
	a.localMu.Unlock()
	// UpdateNode re-sends the alive message with fresh delegate meta.
	if err := a.ml.UpdateNode(5 * time.Second); err != nil {
		return errors.New(errors.KindUnavailable, "membership.UpdateMeta", "meta update: "+err.Error())
	}
	return nil
}

// Leave performs a graceful departure (announces the leave to peers).
func (a *Agent) Leave(timeout time.Duration) error {
	return a.ml.Leave(timeout)
}

// Shutdown stops the node's memberlist without announcing a leave: peers
// must time the node out through probing (~3s). This is the crash
// semantics a real power-loss produces.
func (a *Agent) Shutdown() error {
	return a.ml.Shutdown()
}

// agentDelegate implements memberlist.Delegate + EventDelegate.
type agentDelegate struct {
	agent *Agent
}

// NodeMeta returns this node's gossiped metadata, re-marshaled from the
// current local meta (called on every alive broadcast; JSON marshal of a
// sub-512-byte struct is negligible at gossip rates).
func (d *agentDelegate) NodeMeta(limit int) []byte {
	d.agent.localMu.RLock()
	m := d.agent.local
	d.agent.localMu.RUnlock()
	b, err := m.Marshal()
	if err != nil || len(b) > limit {
		return nil // Marshal enforces the limit; never happens
	}
	return b
}

func (d *agentDelegate) NotifyMsg([]byte) {}

func (d *agentDelegate) GetBroadcasts(int, int) [][]byte { return nil }

func (d *agentDelegate) LocalState(bool) []byte { return nil }

func (d *agentDelegate) MergeRemoteState([]byte, bool) {}

// record updates the local node-state store. memberlist is LIVENESS only:
// this records what gossip believes, nothing else. Higher layers decide —
// through quorum — whether to act on it.
func (a *Agent) record(id string, st NodeState) {
	a.mu.Lock()
	prev, had := a.states[id]
	if had {
		st.Since = prev.Since // state-change bookkeeping below resets it
	}
	a.states[id] = &st
	a.mu.Unlock()

	if a.conf.OnEvent != nil {
		kind := EventUpdate
		switch {
		case !had:
			kind = EventJoin
		case prev.State != st.State:
			kind = EventUpdate
		}
		a.conf.OnEvent(Event{Kind: kind, Node: st.Meta})
	}
}

// markLeft records a graceful departure or a confirmed death.
func (a *Agent) markLeft(id string, meta NodeMeta, addr string) {
	a.mu.Lock()
	prev, had := a.states[id]
	a.states[id] = &NodeState{Meta: meta, Addr: addr, State: "dead", Since: time.Now()}
	a.mu.Unlock()

	if had && prev.State == "dead" {
		return // already reported
	}
	if a.conf.OnEvent != nil {
		a.conf.OnEvent(Event{Kind: EventLeave, Node: meta})
	}
}

func stateString(s memberlist.NodeStateType) string {
	switch s {
	case memberlist.StateAlive:
		return "alive"
	case memberlist.StateSuspect:
		return "suspect"
	default:
		return "dead"
	}
}

// NotifyJoin is invoked when a node is detected to have joined.
func (d *agentDelegate) NotifyJoin(n *memberlist.Node) {
	meta := unmarshalNodeMeta(n.Meta)
	if meta.NodeID == "" {
		meta.NodeID = n.Name
	}
	d.agent.record(meta.NodeID, NodeState{
		Meta:  meta,
		Addr:  n.Address(),
		State: "alive",
		Since: time.Now(),
	})
}

// NotifyUpdate is invoked when a node's metadata changes.
func (d *agentDelegate) NotifyUpdate(n *memberlist.Node) {
	meta := unmarshalNodeMeta(n.Meta)
	if meta.NodeID == "" {
		meta.NodeID = n.Name
	}
	d.agent.record(meta.NodeID, NodeState{
		Meta:  meta,
		Addr:  n.Address(),
		State: stateString(n.State),
		Since: time.Now(),
	})
}

// NotifyLeave is invoked when a node is detected to have left — whether
// graceful or confirmed dead after suspicion. It is a LIVENESS signal only.
func (d *agentDelegate) NotifyLeave(n *memberlist.Node) {
	meta := unmarshalNodeMeta(n.Meta)
	if meta.NodeID == "" {
		meta.NodeID = n.Name
	}
	d.agent.markLeft(meta.NodeID, meta, n.Address())
}
