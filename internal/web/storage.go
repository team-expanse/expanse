// Storage per node: the card on a node's page shows what the node reports
// (storage.NodeStorage), and its form asks the node to turn storage on.
package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

// nodeStorageView is the node page's Storage card.
type nodeStorageView struct {
	Node    string
	Report  *storage.NodeStorage
	Pending bool // asked for, the node has not acted yet
	Percent string
	Error   string
}

// CanEnable says the form is offered: the VG has free space and storage is off or failed.
func (v nodeStorageView) CanEnable() bool {
	r := v.Report
	if r == nil || r.VGFree == 0 || v.Pending {
		return false
	}
	return r.State == storage.NodeStorageOff || r.State == storage.NodeStorageFailed
}

func (s *Server) nodeStorageView(ctx context.Context, node string) nodeStorageView {
	ctx = store.WithStale(ctx)
	v := nodeStorageView{Node: node, Percent: strconv.Itoa(storage.DefaultPercent)}
	if reps, err := storage.ListNodeStorage(ctx, s.store); err == nil {
		if r, ok := reps[node]; ok {
			v.Report = &r
		}
	}
	req, ok, _ := storage.LoadEnableRequest(ctx, s.store, node)
	if ok {
		v.Percent = strconv.Itoa(req.WithDefaults().Percent)
		v.Pending = v.Report != nil && v.Report.State == storage.NodeStorageOff
	}
	return v
}

func (s *Server) handleNodeStorageEnable(w http.ResponseWriter, r *http.Request) {
	data, ok := s.nodeDetail(w, r)
	if !ok {
		return
	}
	percent := strings.TrimSuffix(strings.TrimSpace(r.FormValue("percent")), "%")
	fail := func(msg string) {
		data.Storage.Percent, data.Storage.Error = percent, msg
		s.render(w, http.StatusBadRequest, "node_detail.html", data)
	}
	if !data.Storage.CanEnable() {
		fail("This node has no free volume group to build storage in.")
		return
	}
	n, err := strconv.Atoi(percent)
	if err != nil || n < 1 || n > storage.MaxPercent {
		fail(fmt.Sprintf("Use 1 to %d%% of the free space; the rest is headroom for the pool's metadata and snapshots.", storage.MaxPercent))
		return
	}
	req := storage.EnableRequest{VG: data.Storage.Report.VG, Pool: storage.DefaultPool, Percent: n}
	if err := storage.SaveEnableRequest(r.Context(), s.store, data.Node.ID, req); err != nil {
		fail(sentence(errText(err)))
		return
	}
	s.done(w, r, "success", "Turning storage on for "+data.Node.ID+". It is ready within a few seconds.", "/nodes/"+data.Node.ID+"#storage")
}

// anyNodeStorage says some node holds volumes, or that no node reports (agents before 1.11).
func (s *Server) anyNodeStorage(ctx context.Context) bool {
	reps, err := storage.ListNodeStorage(store.WithStale(ctx), s.store)
	if err != nil || len(reps) == 0 {
		return true
	}
	for _, r := range reps {
		if r.State == storage.NodeStorageReady {
			return true
		}
	}
	return false
}

// Pill is the card's state badge.
func (v nodeStorageView) Pill() pill {
	switch {
	case v.Report == nil:
		return pill{Label: "Unknown", Tone: "neutral"}
	case v.Pending:
		return pill{Label: "Turning on", Tone: "info"}
	}
	switch v.Report.State {
	case storage.NodeStorageReady:
		return pill{Label: "On", Tone: "ok"}
	case storage.NodeStorageReboot:
		return pill{Label: "Reboot needed", Tone: "warn"}
	case storage.NodeStorageFailed:
		return pill{Label: "Failed", Tone: "crit"}
	}
	return pill{Label: "Off", Tone: "neutral"}
}
