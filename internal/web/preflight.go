package web

import (
	"context"
	"fmt"
	"html/template"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/blocks/service"
	"github.com/expanse/expanse/internal/scheduler"
	pb "github.com/expanse/expanse/proto"
)

// blockPlanner is the block service's dry-run placement (service.Server.Preflight).
type blockPlanner interface {
	Preflight(ctx context.Context, b *pb.Block) ([]service.Planned, error)
}

type preflightReplica struct {
	Label    string
	Node     string
	Problems []string // "node: detail", when no node takes it
	Hint     template.HTML
}

// preflightView is the form's pre-flight card: where each replica would run and what clashes.
type preflightView struct {
	Replicas []preflightReplica
	Ports    []string
}

// Fits reports that every replica found a node and no port clashes.
func (v *preflightView) Fits() bool {
	for _, r := range v.Replicas {
		if r.Node == "" {
			return false
		}
	}
	return len(v.Ports) == 0
}

// preflightHints say, in the form's own terms, what to change when every node refuses for one reason.
var preflightHints = map[string]template.HTML{
	scheduler.CodeInsufficientMemory: "Lower <b>Memory</b>, or add memory to a node.",
	scheduler.CodeInsufficientCPU:    "Lower <b>CPU</b>, or join another node.",
	scheduler.CodeInsufficientDisk:   "Shrink the volume sizes, or free disk on a node.",
	scheduler.CodeMissingCapability:  "No node has what this type needs; join one that does.",
	scheduler.CodeAntiAffinity:       "Each replica needs its own node: lower <b>Replicas</b>, or join more nodes.",
	scheduler.CodeNotReady:           `No node takes blocks right now; see <a href="/nodes">Nodes</a>.`,
	scheduler.CodeNoNodes:            `No node takes blocks right now; see <a href="/nodes">Nodes</a>.`,
}

// preflight dry-runs b's placement and port use; nil when the service cannot plan.
func (s *Server) preflight(ctx context.Context, b *pb.Block) *preflightView {
	p, ok := s.blocks.(blockPlanner)
	if !ok {
		return nil
	}
	plan, err := p.Preflight(ctx, b)
	if err != nil {
		return nil
	}
	v := &preflightView{Ports: portClashes(b, s.usedPorts(ctx))}
	for _, r := range plan {
		pr := preflightReplica{Label: replicaLabel(r.Replica, len(plan)), Node: r.Node, Hint: preflightHints[r.Code]}
		for _, node := range sortedKeys(r.Rejected) {
			pr.Problems = append(pr.Problems, problem(s.names.name(node), r.Rejected[node]))
		}
		v.Replicas = append(v.Replicas, pr)
	}
	return v
}

// problem words a scheduler refusal ("Code: detail") for the form.
func problem(node, reason string) string {
	code, detail, _ := strings.Cut(reason, ": ")
	if code == scheduler.CodeNotReady {
		detail = "not ready to take blocks"
		if strings.Contains(reason, "cordoned=true") {
			detail = "cordoned"
		}
	}
	return node + ": " + detail
}

func replicaLabel(i, n int) string {
	switch {
	case i < 0:
		return "A copy"
	case n == 1:
		return "The block"
	}
	return fmt.Sprintf("Replica %d", i+1)
}

// portClashes names the existing blocks that listen on a port b would too.
func portClashes(b *pb.Block, used map[int32]string) []string {
	var out []string
	for _, p := range blockPorts(b) {
		if owner, ok := used[p]; ok {
			out = append(out, fmt.Sprintf("Port %d is also used by %s; the two cannot run on the same node.", p, owner))
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
