package web

import (
	"time"

	pb "github.com/expanse/expanse/proto"
)

// replicaView is one placement row on the block page: its readiness, restarts and last probe.
type replicaView struct {
	Index    int32
	Live     bool // false for a placement the controller stopped and replaced
	Ready    pill
	Restarts int32
	Last     string
	At       time.Time
}

func replicaRow(p *pb.PlacementStatus) replicaView {
	if p.GetReplicaIndex() < 0 {
		last := p.GetMessage()
		if last == "" && p.GetPhase() == pb.Phase_LOST {
			last = "node lost"
		}
		return replicaView{Index: p.GetFormerIndex(), Last: last}
	}
	h := p.GetHealth()
	v := replicaView{Index: p.GetReplicaIndex(), Live: true, Ready: pill{"Unknown", "neutral"}, Restarts: h.GetRestarts()}
	if r := h.GetReadiness(); r != nil {
		v.Ready = map[bool]pill{true: {"Ready", "ok"}, false: {"Not ready", "warn"}}[r.GetOk()]
	}
	r, l := h.GetReadiness(), h.GetLiveness()
	switch {
	case p.GetMessage() != "":
		v.Last = p.GetMessage()
	case l != nil && !l.GetOk():
		v.Last, v.At = l.GetDetail(), time.Unix(0, l.GetAtUnixNs())
	case r != nil:
		v.Last, v.At = r.GetDetail(), time.Unix(0, r.GetAtUnixNs())
	case l != nil:
		v.Last, v.At = l.GetDetail(), time.Unix(0, l.GetAtUnixNs())
	}
	return v
}

// livePlacements counts the placements still serving a replica.
func livePlacements(s *pb.BlockStatus) int {
	n := 0
	for _, p := range s.GetPlacements() {
		if p.GetReplicaIndex() >= 0 {
			n++
		}
	}
	return n
}
