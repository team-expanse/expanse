// Stream D1: thin handlers over the same store keys `expanse ctl volume`
// writes and reads (internal/storage's Spec/Status records, plus the
// pending-create and operator-request keys the leader's volume
// controller/runtimes consume) — D1, in-process, directly against the
// store this node already holds, never a second implementation of the
// controller's own logic.
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// volumeOpsPrefix must match the literal the volume controller/runtimes
// scan (internal/storage/controller's opsPrefix, internal/storage/volume's
// opKey — both unexported, so this mirrors cmd_volume.go's own
// volumePendingKeyPrefix: the ctl and the UI are each a client of the
// store, not the controller package itself).
const volumeOpsPrefix = "/volumes/_ops/"

func (s *Server) registerVolumeRoutes() {
	s.mux.Handle("GET /volumes", s.requireAuth(http.HandlerFunc(s.handleVolumesList)))
	s.mux.Handle("GET /volumes/new", s.requireAuth(http.HandlerFunc(s.handleVolumeNew)))
	s.mux.Handle("POST /volumes", s.requireAuth(http.HandlerFunc(s.handleVolumeCreate)))
	s.mux.Handle("GET /volumes/{name}", s.requireAuth(http.HandlerFunc(s.handleVolumeDetail)))
	s.mux.Handle("GET /volumes/{name}/events", s.requireAuth(http.HandlerFunc(s.handleVolumeEvents)))
	s.mux.Handle("POST /volumes/{name}/resize", s.requireAuth(http.HandlerFunc(s.handleVolumeResize)))
	s.mux.Handle("POST /volumes/{name}/snapshot", s.requireAuth(http.HandlerFunc(s.handleVolumeSnapshot)))
	s.mux.Handle("POST /volumes/{name}/delete", s.requireAuth(http.HandlerFunc(s.handleVolumeDelete)))
}

// volumeView is one volume's spec + status + snapshots, the same three
// store records `ctl volume inspect` reads — storage.Spec/Status already
// render their State/Role fields as human strings ("Healthy",
// "Secondary"), so no separate title-casing table is needed here.
type volumeView struct {
	ID          string
	Name        string
	SizeBytes   uint64
	Size        string
	Class       string
	Replication int
	State       storage.VolumeState
	Primary     string
	Placement   []storage.Replica
	Snapshots   []storage.SnapshotRecord
}

// ReplicaSummary is "members of target", flagging a volume that has no redundancy.
func (v volumeView) ReplicaSummary() string {
	s := fmt.Sprintf("%d of %d", len(v.Placement), v.Replication)
	if v.State == storage.StateUnderReplicated {
		s += " (no redundancy)"
	}
	return s
}

func humanBytes(n uint64) string {
	return quantity.Bytes{N: int64(n)}.String()
}

func (s *Server) loadVolumeView(ctx context.Context, id string) (*volumeView, error) {
	spec, err := storage.LoadSpec(ctx, s.store, id)
	if err != nil {
		return nil, err
	}
	st, _, err := storage.LoadStatus(ctx, s.store, id)
	if err != nil && errors.KindOf(err) != errors.KindNotFound {
		return nil, err
	}
	snaps, err := storage.ListSnapshotRecords(ctx, s.store, id)
	if err != nil {
		return nil, err
	}
	return &volumeView{
		ID: spec.ID, Name: spec.Name, SizeBytes: spec.SizeBytes, Size: humanBytes(spec.SizeBytes),
		Class: spec.Class, Replication: spec.Replication,
		State: st.State, Primary: st.Primary, Placement: st.Placement, Snapshots: snaps,
	}, nil
}

// findVolumeByName resolves a volume by its operator-facing name, the
// same lookup cmd_volume_ops.go's resolveVol does against the CLI's own
// (separately loaded) view of the store.
func (s *Server) findVolumeByName(ctx context.Context, name string) (*volumeView, error) {
	ids, err := storage.ListVolumeIDs(ctx, s.store)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		spec, err := storage.LoadSpec(ctx, s.store, id)
		if err == nil && spec.Name == name {
			return s.loadVolumeView(ctx, id)
		}
	}
	return nil, errors.New(errors.KindNotFound, "web.findVolumeByName", "no such volume "+name)
}

// volumePending reports a queued-but-not-yet-placed creation request
// (storage.PendingCreateKey): the window between a create POST and the
// leader's controller assigning an ID and writing the first spec/status.
func (s *Server) volumePending(ctx context.Context, name string) bool {
	_, err := s.store.Get(ctx, storage.PendingCreateKey(name))
	return err == nil
}

func (s *Server) putVolumeOp(ctx context.Context, kind, volID string, val any) error {
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	_, err = s.store.Put(ctx, store.Key(volumeOpsPrefix+kind+"/"+volID), raw)
	return err
}

type volumesListData struct {
	NodeID    string
	CSRFToken string
	Volumes   []*volumeView
}

func (s *Server) handleVolumesList(w http.ResponseWriter, r *http.Request) {
	ids, err := storage.ListVolumeIDs(r.Context(), s.store)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var views []*volumeView
	for _, id := range ids {
		v, err := s.loadVolumeView(r.Context(), id)
		if err != nil {
			continue // a volume mid-delete/creation with a half-written record; skip rather than fail the whole list
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	s.render(w, http.StatusOK, "volumes_list.html", volumesListData{
		NodeID: s.NodeID, CSRFToken: sessionFromContext(r.Context()).CSRFToken, Volumes: views,
	})
}

type volumeNewData struct {
	NodeID, CSRFToken              string
	Name, Size, Class, Replication string
	Error                          string
}

func (s *Server) handleVolumeNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "volume_new.html", volumeNewData{
		NodeID: s.NodeID, CSRFToken: sessionFromContext(r.Context()).CSRFToken,
		Class: "default",
	})
}

func (s *Server) handleVolumeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := r.PostForm.Get("name")
	sizeStr := r.PostForm.Get("size")
	class := r.PostForm.Get("class")
	replStr := r.PostForm.Get("replication")

	fail := func(msg string) {
		s.render(w, http.StatusBadRequest, "volume_new.html", volumeNewData{
			NodeID: s.NodeID, CSRFToken: sessionFromContext(r.Context()).CSRFToken,
			Name: name, Size: sizeStr, Class: class, Replication: replStr, Error: msg,
		})
	}

	if name == "" {
		fail("name is required")
		return
	}
	size, err := quantity.ParseBytes(sizeStr)
	if err != nil {
		fail("--size: " + err.Error())
		return
	}
	repl := 0
	if replStr != "" {
		if repl, err = strconv.Atoi(replStr); err != nil {
			fail("replication must be an integer")
			return
		}
	}
	raw, err := proto.Marshal(&pb.VolumeSpec{Name: name, SizeBytes: uint64(size.N), Class: class, Replication: int32(repl)})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := s.store.Put(r.Context(), storage.PendingCreateKey(name), raw); err != nil {
		fail(err.Error())
		return
	}
	http.Redirect(w, r, "/volumes/"+name, http.StatusSeeOther)
}

type volumeDetailData struct {
	NodeID    string
	CSRFToken string
	Name      string
	// Volume is nil while the creation request is still pending (or, on
	// a stale render, while it has not appeared yet), rendered as a
	// "creating" placeholder rather than a 404.
	Volume *volumeView
}

func (s *Server) handleVolumeDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	v, err := s.findVolumeByName(r.Context(), name)
	if err != nil {
		if errors.KindOf(err) != errors.KindNotFound {
			http.Error(w, err.Error(), httpStatus(err))
			return
		}
		if !s.volumePending(r.Context(), name) {
			http.Error(w, "no such volume", http.StatusNotFound)
			return
		}
		v = nil
	}
	s.render(w, http.StatusOK, "volume_detail.html", volumeDetailData{
		NodeID: s.NodeID, CSRFToken: sessionFromContext(r.Context()).CSRFToken, Name: name, Volume: v,
	})
}

// handleVolumeEvents streams the volume's live state as SSE (X6's
// "replica state" reachable live, matching B1/C1's own SSE pages): every
// write under /volumes/ triggers a full re-render of this one volume's
// fragment, covering both its own spec/status/snapshot changes and its
// pending-create request resolving into a real volume.
func (s *Server) handleVolumeEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	name := r.PathValue("name")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // confirm the stream is live before the first event

	send := func() error {
		data := volumeDetailData{Name: name}
		v, err := s.findVolumeByName(r.Context(), name)
		switch {
		case err == nil:
			data.Volume = v
		case errors.KindOf(err) == errors.KindNotFound:
			// still pending, or gone; render the placeholder either way
		default:
			return nil // transient read error; the next event retries
		}
		var buf bytes.Buffer
		if err := s.tmpl.ExecuteTemplate(&buf, "volume-fragment", data); err != nil {
			return err
		}
		if err := writeSSE(w, "volume", buf.String()); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// Watch before the initial snapshot (B1's fix, applied here from the
	// start): a write landing between a snapshot read and the watch's
	// starting revision would otherwise be delivered by neither.
	cur, err := s.store.Revision(r.Context())
	if err != nil {
		return
	}
	changes, err := s.store.Watch(r.Context(), store.Key(storage.VolumePrefix), cur)
	if err != nil {
		return
	}
	if err := send(); err != nil {
		return
	}
	for range changes {
		if err := send(); err != nil {
			return
		}
	}
}

func (s *Server) handleVolumeResize(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	v, err := s.findVolumeByName(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), httpStatus(err))
		return
	}
	size, err := quantity.ParseBytes(r.PostForm.Get("size"))
	if err != nil {
		http.Error(w, "--size: "+err.Error(), http.StatusBadRequest)
		return
	}
	if uint64(size.N) <= v.SizeBytes {
		http.Error(w, "the new size must be larger than the current "+v.Size+"; volumes only grow", http.StatusBadRequest)
		return
	}
	if err := s.putVolumeOp(r.Context(), "resize", v.ID, map[string]any{"target": name, "sizeBytes": size.N}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Resize is applied asynchronously by the leader's controller; the
	// open SSE connection (or a follow-up GET) shows it once it lands.
	http.Redirect(w, r, "/volumes/"+name, http.StatusSeeOther)
}

func (s *Server) handleVolumeSnapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	v, err := s.findVolumeByName(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), httpStatus(err))
		return
	}
	snapName := r.PostForm.Get("name")
	if err := storage.ValidSnapshotName(snapName); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, existing := range v.Snapshots {
		if existing.Name == snapName {
			http.Error(w, "a snapshot named "+snapName+" already exists, held by "+existing.Node, http.StatusConflict)
			return
		}
	}
	if err := s.putVolumeOp(r.Context(), "snapshot", v.ID, map[string]string{"name": snapName}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/volumes/"+name, http.StatusSeeOther)
}

func (s *Server) handleVolumeDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	v, err := s.findVolumeByName(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), httpStatus(err))
		return
	}
	if err := s.putVolumeOp(r.Context(), "delete", v.ID, map[string]any{"target": name}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Redirect", "/volumes")
	http.Redirect(w, r, "/volumes", http.StatusSeeOther)
}
