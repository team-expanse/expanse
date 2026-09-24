// Package storage holds the volume data model and store persistence for
// replicated volumes (Phase 06 §4.1): the Volume/Replica shapes, their proto-marshaled
// store records at /volumes/<id>/spec and /volumes/<id>/status, and the
// CAS-based update idiom shared with the rest of the tree (blocks, VIPs).
package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// VolumeState is a volume's lifecycle state.
type VolumeState string

const (
	StateCreating  VolumeState = "Creating"
	StateHealthy   VolumeState = "Healthy"
	StateDegraded  VolumeState = "Degraded"
	StateReadOnly  VolumeState = "ReadOnly"
	StateResyncing VolumeState = "Resyncing"
	StateFailed    VolumeState = "Failed"
	StateDeleting  VolumeState = "Deleting"
	// StateNeedsManualRecovery marks split-brain divergence (§9):
	// automatic recovery refused, all copies preserved.
	StateNeedsManualRecovery VolumeState = "NeedsManualRecovery"
)

func (s VolumeState) proto() pb.VolumeState {
	switch s {
	case StateCreating:
		return pb.VolumeState_VOLUME_STATE_CREATING
	case StateHealthy:
		return pb.VolumeState_VOLUME_STATE_HEALTHY
	case StateDegraded:
		return pb.VolumeState_VOLUME_STATE_DEGRADED
	case StateReadOnly:
		return pb.VolumeState_VOLUME_STATE_READONLY
	case StateResyncing:
		return pb.VolumeState_VOLUME_STATE_RESYNCING
	case StateFailed:
		return pb.VolumeState_VOLUME_STATE_FAILED
	case StateDeleting:
		return pb.VolumeState_VOLUME_STATE_DELETING
	case StateNeedsManualRecovery:
		return pb.VolumeState_VOLUME_STATE_NEEDS_MANUAL_RECOVERY
	}
	return pb.VolumeState_VOLUME_STATE_UNSPECIFIED
}

func stateFromProto(p pb.VolumeState) VolumeState {
	switch p {
	case pb.VolumeState_VOLUME_STATE_CREATING:
		return StateCreating
	case pb.VolumeState_VOLUME_STATE_HEALTHY:
		return StateHealthy
	case pb.VolumeState_VOLUME_STATE_DEGRADED:
		return StateDegraded
	case pb.VolumeState_VOLUME_STATE_READONLY:
		return StateReadOnly
	case pb.VolumeState_VOLUME_STATE_RESYNCING:
		return StateResyncing
	case pb.VolumeState_VOLUME_STATE_FAILED:
		return StateFailed
	case pb.VolumeState_VOLUME_STATE_DELETING:
		return StateDeleting
	case pb.VolumeState_VOLUME_STATE_NEEDS_MANUAL_RECOVERY:
		return StateNeedsManualRecovery
	}
	return ""
}

// Role is one replica's position in the replication protocol (§4.3).
type Role string

const (
	RolePrimary   Role = "Primary"
	RoleSecondary Role = "Secondary"
	RoleResyncing Role = "Resyncing"
	RoleStale     Role = "Stale"
)

func (r Role) proto() pb.ReplicaRole {
	switch r {
	case RolePrimary:
		return pb.ReplicaRole_REPLICA_ROLE_PRIMARY
	case RoleSecondary:
		return pb.ReplicaRole_REPLICA_ROLE_SECONDARY
	case RoleResyncing:
		return pb.ReplicaRole_REPLICA_ROLE_RESYNCING
	case RoleStale:
		return pb.ReplicaRole_REPLICA_ROLE_STALE
	}
	return pb.ReplicaRole_REPLICA_ROLE_UNSPECIFIED
}

func roleFromProto(p pb.ReplicaRole) Role {
	switch p {
	case pb.ReplicaRole_REPLICA_ROLE_PRIMARY:
		return RolePrimary
	case pb.ReplicaRole_REPLICA_ROLE_SECONDARY:
		return RoleSecondary
	case pb.ReplicaRole_REPLICA_ROLE_RESYNCING:
		return RoleResyncing
	case pb.ReplicaRole_REPLICA_ROLE_STALE:
		return RoleStale
	}
	return ""
}

// Replica is one placement of a volume (§4.1).
type Replica struct {
	NodeID   string
	Role     Role
	LastSeen time.Time
	Healthy  bool
	// SyncPercent is a resync's progress into this replica, zero when none runs.
	SyncPercent float64
	// OutOfSyncKiB is what the primary's kernel counts as differing from this replica.
	OutOfSyncKiB uint64
	// Verifying marks a verify comparing this replica with the primary's.
	Verifying bool
}

// Volume is the spec + live status of one replicated volume (§4.1). Spec is
// written once by the controller; Status is CAS-updated as replicas and
// the primary evolve.
type Volume struct {
	ID          string // "vol-" + 16 hex
	Name        string
	Namespace   string
	SizeBytes   uint64
	Class       string // "default", "fast", "local", "ceph"
	Replication int    // 1..5
	Placement   []Replica
	Generation  uint64      // bumped on every membership change
	State       VolumeState // Creating|Healthy|Degraded|ReadOnly|Resyncing|Failed|Deleting
	Primary     string      // node ID holding the primary lease
}

// Spec is the immutable-except-size configuration half of a Volume.
type Spec struct {
	ID          string
	Name        string
	Namespace   string
	SizeBytes   uint64
	Class       string
	Replication int
}

// Status is the mutable half of a Volume.
type Status struct {
	Generation uint64
	State      VolumeState
	Primary    string
	Placement  []Replica
}

// Spec returns the spec half of the volume.
func (v Volume) Spec() Spec {
	return Spec{
		ID:          v.ID,
		Name:        v.Name,
		Namespace:   v.Namespace,
		SizeBytes:   v.SizeBytes,
		Class:       v.Class,
		Replication: v.Replication,
	}
}

// Status returns the status half of the volume.
func (v Volume) Status() Status {
	return Status{
		Generation: v.Generation,
		State:      v.State,
		Primary:    v.Primary,
		Placement:  append([]Replica(nil), v.Placement...),
	}
}

// PendingPrefix is the store key namespace for pending volume-creation
// requests (Phase 06 T10): `ctl volume create` writes one via the agent
// socket; the leader's volume runtime performs placement and consumes it.
const PendingPrefix = VolumePrefix + "_pending/"

// PendingCreateKey is the store key for a named creation request. The
// value is a marshaled pb.VolumeSpec.
func PendingCreateKey(name string) store.Key { return store.Key(PendingPrefix + name) }

// BlockVolumeName is the cluster volume name auto-provisioned for a
// block's storage entry (blocks attach to volumes by this composite name,
// not the storage entry's own raw name): internal/storage/controller
// creates it, internal/blocks/wire looks it up, and the scheduler's P12
// filter (PHASE-03-TASKS.md D2) gates placement on it — all three must
// agree on the same name, so it lives here rather than in any one of them.
func BlockVolumeName(ns, block, storage string) string {
	return fmt.Sprintf("blk-%s-%s-%s", ns, block, storage)
}

// BlockReplicaVolumeName is the cluster volume name auto-provisioned for
// one replica's OWN independent storage entry (PHASE-05-TASKS.md D3):
// unlike BlockVolumeName's single composite name shared by every
// placement of a SINGLETON/DAEMONSET block, an active-active block with
// bound storage gets one of these per replica index, so N replicas never
// contend over the same volume's DRBD primary.
func BlockReplicaVolumeName(ns, block, storage string, idx int) string {
	return fmt.Sprintf("%s-%d", BlockVolumeName(ns, block, storage), idx)
}

// VolumePrefix is the store key namespace for volumes (§4.1).
const VolumePrefix = "/volumes/"

// SpecKey is the store key for a volume's spec record.
func SpecKey(volID string) store.Key { return store.Key(VolumePrefix + volID + "/spec") }

// StatusKey is the store key for a volume's status record.
func StatusKey(volID string) store.Key { return store.Key(VolumePrefix + volID + "/status") }

// SpecToProto marshals a spec into its store representation.
func SpecToProto(s Spec) *pb.VolumeSpec {
	return &pb.VolumeSpec{
		Id:          s.ID,
		Name:        s.Name,
		Namespace:   s.Namespace,
		SizeBytes:   s.SizeBytes,
		Class:       s.Class,
		Replication: int32(s.Replication),
	}
}

// SpecFromProto unmarshals a spec from its store representation.
func SpecFromProto(p *pb.VolumeSpec) Spec {
	return Spec{
		ID:          p.GetId(),
		Name:        p.GetName(),
		Namespace:   p.GetNamespace(),
		SizeBytes:   p.GetSizeBytes(),
		Class:       p.GetClass(),
		Replication: int(p.GetReplication()),
	}
}

func replicaToProto(r Replica) *pb.Replica {
	return &pb.Replica{
		NodeId:           r.NodeID,
		Role:             r.Role.proto(),
		LastSeenUnixNano: r.LastSeen.UnixNano(),
		Healthy:          r.Healthy,
		SyncPercent:      r.SyncPercent,
		OutOfSyncKib:     r.OutOfSyncKiB,
		Verifying:        r.Verifying,
	}
}

func replicaFromProto(p *pb.Replica) Replica {
	return Replica{
		NodeID:       p.GetNodeId(),
		Role:         roleFromProto(p.GetRole()),
		LastSeen:     time.Unix(0, p.GetLastSeenUnixNano()).UTC(),
		Healthy:      p.GetHealthy(),
		SyncPercent:  p.GetSyncPercent(),
		OutOfSyncKiB: p.GetOutOfSyncKib(),
		Verifying:    p.GetVerifying(),
	}
}

// StatusToProto marshals a status into its store representation.
func StatusToProto(s Status) *pb.VolumeStatus {
	p := &pb.VolumeStatus{
		Generation: s.Generation,
		State:      s.State.proto(),
		Primary:    s.Primary,
	}
	for _, r := range s.Placement {
		p.Placement = append(p.Placement, replicaToProto(r))
	}
	return p
}

// StatusFromProto unmarshals a status from its store representation.
func StatusFromProto(p *pb.VolumeStatus) Status {
	s := Status{
		Generation: p.GetGeneration(),
		State:      stateFromProto(p.GetState()),
		Primary:    p.GetPrimary(),
	}
	for _, r := range p.GetPlacement() {
		s.Placement = append(s.Placement, replicaFromProto(r))
	}
	return s
}

// SaveSpec writes a volume spec to the store.
func SaveSpec(ctx context.Context, st store.Store, s Spec) error {
	raw, err := proto.Marshal(SpecToProto(s))
	if err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "storage.SaveSpec", "marshal")
	}
	if _, err := st.Put(ctx, SpecKey(s.ID), raw); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "storage.SaveSpec", "put")
	}
	return nil
}

// LoadSpec reads a volume spec; KindNotFound if the volume doesn't exist.
func LoadSpec(ctx context.Context, st store.Store, volID string) (Spec, error) {
	entry, err := st.Get(ctx, SpecKey(volID))
	if err != nil {
		return Spec{}, experrors.Wrap(err, experrors.KindNotFound, "storage.LoadSpec", "get")
	}
	var p pb.VolumeSpec
	if err := proto.Unmarshal(entry.Value, &p); err != nil {
		return Spec{}, experrors.Wrap(err, experrors.KindInternal, "storage.LoadSpec", "unmarshal")
	}
	return SpecFromProto(&p), nil
}

// LoadSpecRev reads a volume spec together with its store revision, for
// a later CompareAndSwapSpec update (the "except size" half of Spec's
// own doc comment — G6.14 online resize).
func LoadSpecRev(ctx context.Context, st store.Store, volID string) (Spec, store.Revision, error) {
	entry, err := st.Get(ctx, SpecKey(volID))
	if err != nil {
		return Spec{}, 0, experrors.Wrap(err, experrors.KindNotFound, "storage.LoadSpecRev", "get")
	}
	var p pb.VolumeSpec
	if err := proto.Unmarshal(entry.Value, &p); err != nil {
		return Spec{}, 0, experrors.Wrap(err, experrors.KindInternal, "storage.LoadSpecRev", "unmarshal")
	}
	return SpecFromProto(&p), entry.Revision, nil
}

// CompareAndSwapSpec updates a spec only if its revision is still
// expect; returns KindConflict on a lost race.
func CompareAndSwapSpec(ctx context.Context, st store.Store, volID string, expect store.Revision, s Spec) error {
	raw, err := proto.Marshal(SpecToProto(s))
	if err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "storage.CompareAndSwapSpec", "marshal")
	}
	if _, err := st.CompareAndSwap(ctx, SpecKey(volID), expect, raw); err != nil {
		return experrors.Wrap(err, experrors.KindConflict, "storage.CompareAndSwapSpec", "cas")
	}
	return nil
}

// SaveStatus writes a volume status unconditionally (controller-owned
// fields only; replicas go through CompareAndSwapStatus).
func SaveStatus(ctx context.Context, st store.Store, volID string, s Status) error {
	raw, err := proto.Marshal(StatusToProto(s))
	if err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "storage.SaveStatus", "marshal")
	}
	if _, err := st.Put(ctx, StatusKey(volID), raw); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "storage.SaveStatus", "put")
	}
	return nil
}

// LoadStatus reads a volume's status and the revision of its record, for
// later CompareAndSwapStatus calls. KindNotFound if the volume doesn't
// exist.
func LoadStatus(ctx context.Context, st store.Store, volID string) (Status, store.Revision, error) {
	entry, err := st.Get(ctx, StatusKey(volID))
	if err != nil {
		return Status{}, 0, experrors.Wrap(err, experrors.KindNotFound, "storage.LoadStatus", "get")
	}
	var p pb.VolumeStatus
	if err := proto.Unmarshal(entry.Value, &p); err != nil {
		return Status{}, 0, experrors.Wrap(err, experrors.KindInternal, "storage.LoadStatus", "unmarshal")
	}
	return StatusFromProto(&p), entry.Revision, nil
}

// CompareAndSwapStatus updates the status only if its revision is still
// expect; returns KindConflict on a lost race (caller re-reads and
// retries — the standard idiom in this tree).
func CompareAndSwapStatus(ctx context.Context, st store.Store, volID string, expect store.Revision, s Status) error {
	raw, err := proto.Marshal(StatusToProto(s))
	if err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "storage.CompareAndSwapStatus", "marshal")
	}
	if _, err := st.CompareAndSwap(ctx, StatusKey(volID), expect, raw); err != nil {
		return experrors.Wrap(err, experrors.KindConflict, "storage.CompareAndSwapStatus", "cas")
	}
	return nil
}

// ListVolumeIDs returns every volume ID present in the store.
func ListVolumeIDs(ctx context.Context, st store.Store) ([]string, error) {
	entries, err := st.List(ctx, VolumePrefix)
	if err != nil {
		return nil, experrors.Wrap(err, experrors.KindInternal, "storage.ListVolumeIDs", "list")
	}
	// Keys are /volumes/<id>/spec|status — collect unique IDs.
	seen := map[string]bool{}
	var ids []string
	for _, e := range entries {
		rest := strings.TrimPrefix(string(e.Key), VolumePrefix)
		if i := len(rest) - len("/spec"); i > 0 && rest[i:] == "/spec" {
			id := rest[:i]
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}
