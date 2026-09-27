// Generations (§4.7): the desired-state history, a diff between any two,
// and rollback — thin handlers over the same NodeService RPCs
// `expanse ctl generation` uses.
package web

import (
	"net/http"
	"strconv"

	"github.com/expanse/expanse/internal/errors"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) registerGenerationRoutes() {
	s.routes.Handle("GET /generations", s.requireAuth(http.HandlerFunc(s.handleGenerationsList)))
	s.routes.Handle("GET /generations/diff", s.requireAuth(http.HandlerFunc(s.handleGenerationsDiff)))
	s.routes.Handle("GET /generations/{n}", s.requireAuth(http.HandlerFunc(s.handleGenerationDetail)))
	s.routes.Handle("POST /generations/{n}/rollback", s.requireAuth(http.HandlerFunc(s.handleGenerationRollback)))
}

// rpcStatus maps a NodeService error (a gRPC status from api.mapErr, or
// an internal/errors kind) to an HTTP status.
func rpcStatus(err error) int {
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.NotFound:
			return http.StatusNotFound
		case codes.InvalidArgument:
			return http.StatusBadRequest
		case codes.Aborted, codes.AlreadyExists:
			return http.StatusConflict
		case codes.Unavailable, codes.FailedPrecondition:
			return http.StatusServiceUnavailable
		}
	}
	if errors.KindOf(err) != errors.KindInternal {
		return httpStatus(err)
	}
	return http.StatusInternalServerError
}

type generationsListData struct {
	Page        page
	Generations []*pb.GenerationInfo // newest first
	Current     uint64
	Error       string
}

func (s *Server) handleGenerationsList(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	data := generationsListData{Page: s.newPage(w, r, "Generations", "generations", crumb{Label: "Generations"})}
	resp, err := s.cluster.ListGenerations(r.Context(), &pb.ListGenerationsRequest{})
	if err != nil {
		data.Error = errText(err)
	}
	gens := resp.GetGenerations()
	for i := len(gens) - 1; i >= 0; i-- {
		data.Generations = append(data.Generations, gens[i])
	}
	if rep, err := s.getClusterReport(r.Context()); err == nil {
		data.Current = uint64(rep.Generation)
	}
	s.render(w, http.StatusOK, "generations_list.html", data)
}

type generationDetailData struct {
	Page       page
	Generation *pb.GenerationInfo
	Current    uint64
}

func (s *Server) handleGenerationDetail(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	n, err := strconv.ParseUint(r.PathValue("n"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Generation numbers are integers.")
		return
	}
	g, err := s.cluster.GetGeneration(r.Context(), &pb.GetGenerationRequest{Number: n, IncludeKeys: true})
	if err != nil {
		s.renderError(w, r, rpcStatus(err), errText(err))
		return
	}
	label := "Generation " + r.PathValue("n")
	data := generationDetailData{
		Page:       s.newPage(w, r, label, "generations", crumb{Label: "Generations", Href: "/generations"}, crumb{Label: label}),
		Generation: g,
	}
	if rep, err := s.getClusterReport(r.Context()); err == nil {
		data.Current = uint64(rep.Generation)
	}
	s.render(w, http.StatusOK, "generation_detail.html", data)
}

type generationsDiffData struct {
	Page page
	A, B uint64
	Diff *pb.DiffGenerationsResponse
}

func (s *Server) handleGenerationsDiff(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	a, errA := strconv.ParseUint(r.URL.Query().Get("a"), 10, 64)
	b, errB := strconv.ParseUint(r.URL.Query().Get("b"), 10, 64)
	if errA != nil || errB != nil {
		s.renderError(w, r, http.StatusBadRequest, "Pick two generation numbers to compare.")
		return
	}
	diff, err := s.cluster.DiffGenerations(r.Context(), &pb.DiffGenerationsRequest{A: a, B: b})
	if err != nil {
		s.renderError(w, r, rpcStatus(err), errText(err))
		return
	}
	label := "Diff " + r.URL.Query().Get("a") + " → " + r.URL.Query().Get("b")
	s.render(w, http.StatusOK, "generations_diff.html", generationsDiffData{
		Page: s.newPage(w, r, label, "generations", crumb{Label: "Generations", Href: "/generations"}, crumb{Label: label}),
		A:    a, B: b, Diff: diff,
	})
}

// handleGenerationRollback restores generation n as a new generation
// (append-only, like `expanse ctl generation rollback`); the confirm
// dialog lives in the template, this only performs the action.
func (s *Server) handleGenerationRollback(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	n, err := strconv.ParseUint(r.PathValue("n"), 10, 64)
	if err != nil {
		http.Error(w, "generation numbers are integers", http.StatusBadRequest)
		return
	}
	resp, err := s.cluster.RollbackGeneration(r.Context(), &pb.RollbackGenerationRequest{Target: n})
	if err != nil {
		http.Error(w, errText(err), rpcStatus(err))
		return
	}
	s.done(w, r, "success", "Rolled back to generation "+r.PathValue("n")+" as generation "+strconv.FormatUint(resp.GetNewGeneration(), 10), "/generations")
}
