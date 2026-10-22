package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/osgen"
	"github.com/expanse/expanse/internal/store"
)

// maxKeep bounds the cleanup form; a node rarely holds more generations than this.
const maxKeep = 100

// nodeGenerationsView is a node's system generations card and its cleanup form.
type nodeGenerationsView struct {
	Node    string
	Report  *osgen.Report
	Running *osgen.Generation // the generation the node runs, nil before it reports
	Keep    string
	Pending bool // a cleanup request the node has not taken yet
	Error   string
}

// Drift is the running generation's release when it is not the system its upgrade built.
func (v nodeGenerationsView) Drift() string {
	if v.Report == nil {
		return ""
	}
	return osgen.Drift(v.Report.Generations)
}

// RemovedText summarizes the last cleanup.
func (v nodeGenerationsView) RemovedText() string {
	c := v.Report.Clean
	switch n := len(c.Removed); {
	case c.Error != "":
		return "The last cleanup failed: " + c.Error
	case n == 0:
		return fmt.Sprintf("The last cleanup kept the newest %d and found nothing to remove.", c.Keep)
	case n == 1:
		return fmt.Sprintf("Removed 1 generation (%d), keeping the newest %d.", c.Removed[0], c.Keep)
	default:
		return fmt.Sprintf("Removed %d generations, keeping the newest %d.", n, c.Keep)
	}
}

func (s *Server) nodeGenerationsView(ctx context.Context, node string) nodeGenerationsView {
	v := nodeGenerationsView{Node: node, Keep: strconv.Itoa(osgen.DefaultKeep)}
	if e, err := s.store.Get(store.WithStale(ctx), osgen.ReportKey(node)); err == nil {
		var rep osgen.Report
		if json.Unmarshal(e.Value, &rep) == nil {
			v.Report = &rep
			for i := range rep.Generations {
				if rep.Generations[i].Current {
					v.Running = &rep.Generations[i]
				}
			}
		}
	}
	if _, err := s.store.Get(store.WithStale(ctx), osgen.RequestKey(node)); err == nil {
		v.Pending = true
	}
	return v
}

func (s *Server) handleNodeGenerationsClean(w http.ResponseWriter, r *http.Request) {
	data, ok := s.nodeDetail(w, r)
	if !ok {
		return
	}
	data.Tab = "generations"
	keep := strings.TrimSpace(r.FormValue("keep"))
	n, err := strconv.Atoi(keep)
	if err != nil || n < 1 || n > maxKeep {
		data.Generations.Keep, data.Generations.Error = keep, fmt.Sprintf("Keep 1 to %d generations; the running and booted ones always stay.", maxKeep)
		s.render(w, http.StatusBadRequest, "node_detail.html", data)
		return
	}
	b, _ := json.Marshal(osgen.CleanRequest{Keep: n})
	if _, err := s.store.Put(r.Context(), osgen.RequestKey(data.Node.ID), b); err != nil {
		data.Generations.Keep, data.Generations.Error = keep, sentence(errText(err))
		s.render(w, http.StatusBadRequest, "node_detail.html", data)
		return
	}
	s.done(w, r, "success", "Cleaning up "+s.names.name(data.Node.ID)+"'s system generations.", "/nodes/"+data.Node.ID+"?tab=generations")
}
