package main

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"text/tabwriter"
	"time"

	pb "github.com/expanse/expanse/proto"
)

// printBlockStatus writes a block's phase, why it is not fully placed, and each replica's health.
func printBlockStatus(w io.Writer, b *pb.Block, now time.Time) {
	m, s := b.GetMetadata(), b.GetStatus()
	fmt.Fprintf(w, "Block:   %s/%s\n", m.GetNamespace(), m.GetName())
	fmt.Fprintf(w, "Type:    %s\n", b.GetSpec().GetType())
	fmt.Fprintf(w, "Phase:   %s (%d/%d ready)\n", s.GetPhase(), s.GetReplicas().GetReady(), b.GetSpec().GetReplicas())
	if r := s.GetPendingReason(); r != nil {
		fmt.Fprintf(w, "Reason:  %s: %s\n", r.GetCode(), r.GetMessage())
		nodes := make([]string, 0, len(r.GetPerNode()))
		for n := range r.GetPerNode() {
			nodes = append(nodes, n)
		}
		slices.Sort(nodes)
		for _, n := range nodes {
			fmt.Fprintf(w, "  %s: %s\n", n, r.GetPerNode()[n])
		}
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPLICA\tNODE\tPHASE\tREADY\tRESTARTS\tLAST PROBE")
	for _, p := range s.GetPlacements() {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", replicaOf(p), p.GetNodeId(), p.GetPhase(), replicaHealthCols(p, now))
	}
	_ = tw.Flush()
}

// replicaOf names the replica a placement serves, or served before it was retired.
func replicaOf(p *pb.PlacementStatus) string {
	if p.GetReplicaIndex() < 0 {
		return strconv.Itoa(int(p.GetFormerIndex()))
	}
	return strconv.Itoa(int(p.GetReplicaIndex()))
}

// replicaHealthCols renders the READY, RESTARTS and LAST PROBE columns; "?" is unknown.
func replicaHealthCols(p *pb.PlacementStatus, now time.Time) string {
	if p.GetReplicaIndex() < 0 {
		why := p.GetMessage()
		if why == "" && p.GetPhase() == pb.Phase_LOST {
			why = "node lost"
		} else if why == "" {
			why = "replaced"
		}
		return "-\t-\tstopped: " + why
	}
	h := p.GetHealth()
	ready := "?"
	if r := h.GetReadiness(); r != nil {
		ready = map[bool]string{true: "yes", false: "no"}[r.GetOk()]
	}
	last := "-"
	switch r, l := h.GetReadiness(), h.GetLiveness(); {
	case p.GetMessage() != "":
		last = p.GetMessage()
	case l != nil && !l.GetOk():
		last = probeText(l, now)
	case r != nil:
		last = probeText(r, now)
	case l != nil:
		last = probeText(l, now)
	}
	return fmt.Sprintf("%s\t%d\t%s", ready, h.GetRestarts(), last)
}

func probeText(r *pb.ProbeResult, now time.Time) string {
	ago := now.Sub(time.Unix(0, r.GetAtUnixNs())).Round(time.Second)
	return fmt.Sprintf("%s (%s ago)", r.GetDetail(), ago)
}
