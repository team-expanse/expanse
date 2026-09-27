// Stream C1: thin HTTP handlers over the same in-process BlockService/
// CatalogService the local gRPC socket already serves (D1) — list,
// deploy (via the same YAML manifest `expanse ctl block apply -f`
// accepts), scale, delete, and live status/log tailing over SSE
// (htmx-sse), never a polling loop.
package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/blocks/apply"
	"github.com/expanse/expanse/internal/errors"
	pb "github.com/expanse/expanse/proto"
)

// registerBlockRoutes wires the /blocks tree, every route behind
// requireAuth (X3). Go 1.22 ServeMux path patterns route by method and
// segment count, so "/blocks/new" (1 segment) never matches
// "/blocks/{ns}/{name}" (2 segments).
func (s *Server) registerBlockRoutes() {
	s.routes.Handle("GET /blocks", s.requireAuth(http.HandlerFunc(s.handleBlocksList)))
	s.routes.Handle("GET /blocks/new", s.requireAuth(http.HandlerFunc(s.handleBlockNew)))
	s.routes.Handle("POST /blocks", s.requireAuth(http.HandlerFunc(s.handleBlockCreate)))
	s.routes.Handle("GET /blocks/{ns}/{name}", s.requireAuth(http.HandlerFunc(s.handleBlockDetail)))
	s.routes.Handle("GET /blocks/{ns}/{name}/events", s.requireAuth(http.HandlerFunc(s.handleBlockEvents)))
	s.routes.Handle("POST /blocks/{ns}/{name}/scale", s.requireAuth(http.HandlerFunc(s.handleBlockScale)))
	s.routes.Handle("POST /blocks/{ns}/{name}/delete", s.requireAuth(http.HandlerFunc(s.handleBlockDelete)))
	s.routes.Handle("GET /blocks/{ns}/{name}/logs", s.requireAuth(http.HandlerFunc(s.handleBlockLogs)))
	s.routes.Handle("GET /blocks/{ns}/{name}/logs/stream", s.requireAuth(http.HandlerFunc(s.handleBlockLogsStream)))
}

// blocksUnavailable reports the block API being disabled on this node
// (cfg.BlocksCatalog unset) rather than panicking on a nil s.blocks.
func (s *Server) blocksUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.blocks != nil {
		return false
	}
	s.renderError(w, r, http.StatusServiceUnavailable, "The block API is not enabled on this node.")
	return true
}

// httpStatus maps an internal/errors Kind to the closest HTTP status.
func httpStatus(err error) int {
	switch errors.KindOf(err) {
	case errors.KindNotFound:
		return http.StatusNotFound
	case errors.KindConflict:
		return http.StatusConflict
	case errors.KindInvalid:
		return http.StatusBadRequest
	case errors.KindPermission:
		return http.StatusForbidden
	case errors.KindUnavailable, errors.KindTimeout:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

type blocksListData struct {
	Page   page
	Blocks []*pb.Block
	Counts phaseCounts
}

func (s *Server) handleBlocksList(w http.ResponseWriter, r *http.Request) {
	if s.blocksUnavailable(w, r) {
		return
	}
	resp, err := s.blocks.List(r.Context(), &pb.ListBlocksRequest{})
	if err != nil {
		http.Error(w, errText(err), httpStatus(err))
		return
	}
	s.render(w, http.StatusOK, "blocks_list.html", blocksListData{
		Page:   s.newPage(w, r, "Blocks", "blocks", crumb{Label: "Blocks"}),
		Blocks: resp.GetBlocks(), Counts: countPhases(resp.GetBlocks()),
	})
}

type blockNewData struct {
	Page    page
	Types   []*pb.BlockType
	Example string
	Error   string
}

// exampleManifest is the deploy form's starting point: a minimal,
// known-valid web/nginx block (nix/blocks/web/nginx/schema.json's only
// required field is serverName).
const exampleManifest = `apiVersion: expanse.io/v1
kind: Block
metadata:
  name: web
  namespace: default
spec:
  type: web/nginx
  replicas: 1
  config:
    serverName: web.example.internal
    port: 8080
`

func (s *Server) handleBlockNew(w http.ResponseWriter, r *http.Request) {
	if s.blocksUnavailable(w, r) {
		return
	}
	var types []*pb.BlockType
	if s.catalog != nil {
		if resp, err := s.catalog.ListTypes(r.Context(), &pb.ListTypesRequest{}); err == nil {
			types = resp.GetTypes()
		}
	}
	s.render(w, http.StatusOK, "block_new.html", blockNewData{
		Page:  s.newPage(w, r, "Deploy a block", "blocks", crumb{Label: "Blocks", Href: "/blocks"}, crumb{Label: "Deploy"}),
		Types: types, Example: exampleManifest,
	})
}

func (s *Server) handleBlockCreate(w http.ResponseWriter, r *http.Request) {
	if s.blocksUnavailable(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	manifest := r.PostForm.Get("manifest")
	b, err := apply.Parse(strings.NewReader(manifest))
	if err != nil {
		s.renderBlockNewError(w, r, manifest, err)
		return
	}
	created, err := s.blocks.Create(r.Context(), b)
	if err != nil {
		s.renderBlockNewError(w, r, manifest, err)
		return
	}
	ns, name := created.GetMetadata().GetNamespace(), created.GetMetadata().GetName()
	s.done(w, r, "success", "Block "+ns+"/"+name+" deployed", "/blocks/"+ns+"/"+name)
}

func (s *Server) renderBlockNewError(w http.ResponseWriter, r *http.Request, manifest string, err error) {
	var types []*pb.BlockType
	if s.catalog != nil {
		if resp, cerr := s.catalog.ListTypes(r.Context(), &pb.ListTypesRequest{}); cerr == nil {
			types = resp.GetTypes()
		}
	}
	s.render(w, http.StatusBadRequest, "block_new.html", blockNewData{
		Page:  s.newPage(w, r, "Deploy a block", "blocks", crumb{Label: "Blocks", Href: "/blocks"}, crumb{Label: "Deploy"}),
		Types: types, Example: manifest, Error: errText(err),
	})
}

type blockDetailData struct {
	Page  page
	Block *pb.Block
}

func (s *Server) getBlock(ctx context.Context, r *http.Request) (*pb.Block, error) {
	return s.blocks.Get(ctx, &pb.GetBlockRequest{Namespace: r.PathValue("ns"), Name: r.PathValue("name")})
}

func (s *Server) handleBlockDetail(w http.ResponseWriter, r *http.Request) {
	if s.blocksUnavailable(w, r) {
		return
	}
	b, err := s.getBlock(r.Context(), r)
	if err != nil {
		s.renderError(w, r, httpStatus(err), errText(err))
		return
	}
	full := b.GetMetadata().GetNamespace() + "/" + b.GetMetadata().GetName()
	s.render(w, http.StatusOK, "block_detail.html", blockDetailData{
		Page:  s.newPage(w, r, full, "blocks", crumb{Label: "Blocks", Href: "/blocks"}, crumb{Label: full}),
		Block: b,
	})
}

// handleBlockEvents streams live status as SSE (X5: "watch it reach
// RUNNING via SSE"), each event a re-rendered "block-fragment" HTML
// swap — HTMX's SSE extension replaces the target element in place, no
// client-side JS and no polling.
func (s *Server) handleBlockEvents(w http.ResponseWriter, r *http.Request) {
	if s.blocksUnavailable(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // confirm the stream is live before the first event, which may be a while

	send := func(b *pb.Block) error {
		var buf bytes.Buffer
		if err := s.renderFragment(&buf, "block-fragment", b); err != nil {
			return err
		}
		if err := writeSSE(w, "block", buf.String()); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// Snapshot first so a client connecting after the terminal state
	// (e.g. already RUNNING) still sees it without waiting on a write.
	if b, err := s.getBlock(r.Context(), r); err == nil {
		if err := send(b); err != nil {
			return
		}
	}

	stream := &blockEventStream{ctx: r.Context(), send: func(ev *pb.BlockEvent) error {
		if ev.GetBlock() == nil {
			return nil
		}
		return send(ev.GetBlock())
	}}
	_ = s.blocks.Watch(&pb.WatchBlocksRequest{Namespace: ns, Name: name}, stream)
}

func (s *Server) handleBlockScale(w http.ResponseWriter, r *http.Request) {
	if s.blocksUnavailable(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	n, err := strconv.ParseInt(r.PostForm.Get("replicas"), 10, 32)
	if err != nil {
		http.Error(w, "replicas must be an integer", http.StatusBadRequest)
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	b, err := s.blocks.Scale(r.Context(), &pb.ScaleRequest{Namespace: ns, Name: name, Replicas: int32(n)})
	if err != nil {
		http.Error(w, errText(err), httpStatus(err))
		return
	}
	// hx-post with hx-target="#block-fragment": return just the updated
	// fragment, not a redirect — the SSE connection covers the rest of
	// this block's lifecycle, this is only the immediate acknowledgement.
	w.Header().Set("HX-Trigger", toastTrigger("success", fmt.Sprintf("Scaling %s/%s to %d", ns, name, n)))
	s.render(w, http.StatusOK, "block-fragment", b)
}

func (s *Server) handleBlockDelete(w http.ResponseWriter, r *http.Request) {
	if s.blocksUnavailable(w, r) {
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if _, err := s.blocks.Delete(r.Context(), &pb.DeleteBlockRequest{Namespace: ns, Name: name}); err != nil {
		http.Error(w, errText(err), httpStatus(err))
		return
	}
	s.done(w, r, "success", "Block "+ns+"/"+name+" deleted", "/blocks")
}

type blockLogsData struct {
	Page      page
	Namespace string
	Name      string
	Replica   int32
	Replicas  []int32
}

func (s *Server) handleBlockLogs(w http.ResponseWriter, r *http.Request) {
	if s.blocksUnavailable(w, r) {
		return
	}
	replica, _ := strconv.ParseInt(r.URL.Query().Get("replica"), 10, 32)
	ns, name := r.PathValue("ns"), r.PathValue("name")
	full := ns + "/" + name
	var replicas []int32
	if b, err := s.getBlock(r.Context(), r); err == nil {
		for _, p := range b.GetStatus().GetPlacements() {
			replicas = append(replicas, p.GetReplicaIndex())
		}
	}
	s.render(w, http.StatusOK, "block_logs.html", blockLogsData{
		Page:      s.newPage(w, r, full+" logs", "blocks", crumb{Label: "Blocks", Href: "/blocks"}, crumb{Label: full, Href: "/blocks/" + full}, crumb{Label: "Logs"}),
		Namespace: ns, Name: name, Replica: int32(replica), Replicas: replicas,
	})
}

// handleBlockLogsStream tails one replica's log via SSE (X5's "tail its
// logs live"), each line its own event so htmx-sse can append it — no
// polling tail. Local-only (§5.5's cross-node proxy, logs.Proxy, is not
// wired anywhere yet, CLI included): a replica hosted on a different
// node than the one answering this request yields an empty stream, not
// an error, matching the CLI's own current reach.
func (s *Server) handleBlockLogsStream(w http.ResponseWriter, r *http.Request) {
	if s.blocksUnavailable(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	replica, _ := strconv.ParseInt(r.URL.Query().Get("replica"), 10, 32)
	req := &pb.LogsRequest{
		Namespace: r.PathValue("ns"), Name: r.PathValue("name"),
		Replica: int32(replica), Follow: true, Tail: 50,
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // confirm the stream is live even before the first log line arrives

	stream := &logLineStream{ctx: r.Context(), send: func(l *pb.LogLine) error {
		// Trailing "\n": EventSource joins multiple "data:" lines (here,
		// the line's text plus one empty trailing line) with "\n", so
		// the reconstructed event carries its own line break — needed
		// since the client appends (hx-swap="beforeend"), not replaces.
		if err := writeSSE(w, "line", l.GetLine()+"\n"); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}}
	_ = s.blocks.StreamLogs(req, stream)
}

// writeSSE encodes one Server-Sent Event, escaping embedded newlines per
// the spec (each line of data needs its own "data:" field).
func writeSSE(w http.ResponseWriter, event, data string) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "event: %s\n", event)
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(&buf, "data: %s\n", line)
	}
	buf.WriteString("\n")
	_, err := w.Write(buf.Bytes())
	return err
}

// phaseCounts is the blocks-by-phase summary the list header and the
// dashboard both show.
type phaseCounts struct {
	Total, Running, Pending, Degraded, Failed int
}

func countPhases(blocks []*pb.Block) phaseCounts {
	var c phaseCounts
	for _, b := range blocks {
		c.Total++
		switch phasePill(b.GetStatus().GetPhase()).Tone {
		case "ok":
			c.Running++
		case "info":
			c.Pending++
		case "warn":
			c.Degraded++
		case "crit":
			c.Failed++
		}
	}
	return c
}
