package drbd

import (
	"context"
	"encoding/json"

	experrors "github.com/expanse/expanse/internal/errors"
)

type (
	Role        string
	DiskState   string
	Connection  string
	Replication string
)

const (
	RolePrimary   Role = "Primary"
	RoleSecondary Role = "Secondary"
	RoleUnknown   Role = "Unknown"

	DiskUpToDate     DiskState = "UpToDate"
	DiskInconsistent DiskState = "Inconsistent"
	DiskUnknown      DiskState = "DUnknown"

	ConnConnected  Connection = "Connected"
	ConnConnecting Connection = "Connecting"
	ConnStandAlone Connection = "StandAlone"

	ReplEstablished Replication = "Established"
	ReplSyncSource  Replication = "SyncSource"
	ReplSyncTarget  Replication = "SyncTarget"
	ReplPausedSyncS Replication = "PausedSyncS"
	ReplPausedSyncT Replication = "PausedSyncT"
)

// Status is one resource as seen from this node.
type Status struct {
	Name    string   `json:"name"`
	NodeID  int      `json:"node-id"`
	Role    Role     `json:"role"`
	Volumes []Volume `json:"devices"`
	Peers   []Peer   `json:"connections"`
}

// Volume is the local side of one replicated device.
type Volume struct {
	Number    int       `json:"volume"`
	Minor     int       `json:"minor"`
	DiskState DiskState `json:"disk-state"`
	Quorum    bool      `json:"quorum"`
	SizeKiB   uint64    `json:"size"`
}

// Peer is one connection to another node.
type Peer struct {
	NodeID     int          `json:"peer-node-id"`
	Name       string       `json:"name"`
	Connection Connection   `json:"connection-state"`
	Role       Role         `json:"peer-role"`
	Volumes    []PeerVolume `json:"peer_devices"`
}

// PeerVolume is a peer's replica of one volume, including resync progress.
type PeerVolume struct {
	Number          int         `json:"volume"`
	Replication     Replication `json:"replication-state"`
	DiskState       DiskState   `json:"peer-disk-state"`
	OutOfSyncKiB    uint64      `json:"out-of-sync"`
	PercentInSync   float64     `json:"percent-in-sync"`
	ResyncSuspended string      `json:"resync-suspended"`
}

// HasQuorum reports whether every local volume has quorum.
func (s *Status) HasQuorum() bool {
	for _, v := range s.Volumes {
		if !v.Quorum {
			return false
		}
	}
	return true
}

// Resyncing reports whether any volume of the peer is being resynchronised.
func (p Peer) Resyncing() bool {
	for _, v := range p.Volumes {
		if v.Replication.resyncing() {
			return true
		}
	}
	return false
}

func (r Replication) resyncing() bool {
	switch r {
	case ReplSyncSource, ReplSyncTarget, ReplPausedSyncS, ReplPausedSyncT:
		return true
	}
	return false
}

// ParseStatus decodes `drbdsetup status --json` output for one resource.
func ParseStatus(res string, out []byte) (*Status, error) {
	var all []Status
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, experrors.Wrap(err, experrors.KindInternal, "drbd.status", "resource "+res+": malformed status JSON")
	}
	if len(all) == 0 {
		return nil, experrors.New(experrors.KindNotFound, "drbd.status", "resource "+res+": not configured")
	}
	return &all[0], nil
}

// Status returns the resource's live state from the kernel.
func (e *Exec) Status(ctx context.Context, res string) (*Status, error) {
	out, err := e.run(ctx, "drbd.status", res, e.DrbdsetupPath, "status", res, "--json", "--verbose", "--statistics")
	if err != nil {
		return nil, err
	}
	return ParseStatus(res, out)
}
