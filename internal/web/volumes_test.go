package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// putTestVolume writes a spec+status directly to the store, bypassing the
// leader controller (not running in these unit tests) — the same
// "write the record the real component would" fixture style
// cluster_test.go uses for a joined node.
func putTestVolume(t *testing.T, st store.Store, spec storage.Spec, status storage.Status) {
	t.Helper()
	if err := storage.SaveSpec(context.Background(), st, spec); err != nil {
		t.Fatalf("SaveSpec: %v", err)
	}
	if err := storage.SaveStatus(context.Background(), st, spec.ID, status); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
}

// postVolumeForm issues a CSRF-protected form POST, the same shape
// blocks_test.go uses (a bare client.PostForm never sets X-CSRF-Token).
func postVolumeForm(t *testing.T, client *http.Client, csrf, targetURL string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, targetURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(csrfHeader, csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", targetURL, err)
	}
	return resp
}

func TestUnauthenticatedVolumesRedirectsToLogin(t *testing.T) {
	srv, _, _ := newClusterTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/volumes")
	if err != nil {
		t.Fatalf("GET /volumes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
}

func TestVolumesListShowsExistingVolumes(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	putTestVolume(t, st,
		storage.Spec{ID: "vol-aaaa", Name: "data", SizeBytes: 10 << 30, Class: "default", Replication: 3},
		storage.Status{State: storage.StateHealthy, Primary: "n1"},
	)

	resp, err := client.Get(srv.URL + "/volumes")
	if err != nil {
		t.Fatalf("GET /volumes: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, "data") || !strings.Contains(text, "Healthy") {
		t.Errorf("list missing volume data/Healthy: %s", text)
	}
}

func TestVolumeCreateWritesPendingSpecAndDetailShowsCreating(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)

	resp := postVolumeForm(t, client, csrf, srv.URL+"/volumes", url.Values{
		"name": {"newvol"}, "size": {"5Gi"}, "class": {"default"}, "replication": {"3"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (after following the redirect to the detail page)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Creating") {
		t.Errorf("detail page did not show the pending placeholder: %s", body)
	}

	e, err := st.Get(context.Background(), storage.PendingCreateKey("newvol"))
	if err != nil {
		t.Fatalf("pending key missing: %v", err)
	}
	var spec pb.VolumeSpec
	if err := proto.Unmarshal(e.Value, &spec); err != nil {
		t.Fatalf("unmarshal pending spec: %v", err)
	}
	if spec.GetName() != "newvol" || spec.GetSizeBytes() != 5<<30 || spec.GetReplication() != 3 {
		t.Errorf("pending spec = %+v, want name=newvol size=5Gi repl=3", &spec)
	}
}

func TestVolumeCreateWithBlankReplicationLeavesItToTheClass(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)
	resp := postVolumeForm(t, client, csrf, srv.URL+"/volumes", url.Values{
		"name": {"newvol"}, "size": {"5Gi"}, "class": {"default"}, "replication": {""},
	})
	resp.Body.Close()
	e, err := st.Get(context.Background(), storage.PendingCreateKey("newvol"))
	if err != nil {
		t.Fatalf("pending key missing (status %d): %v", resp.StatusCode, err)
	}
	var spec pb.VolumeSpec
	if err := proto.Unmarshal(e.Value, &spec); err != nil || spec.GetReplication() != 0 {
		t.Fatalf("pending spec = %+v, %v; want replication unset", &spec, err)
	}
}

func TestUnderReplicatedVolumeSaysNoRedundancy(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	putTestVolume(t, st,
		storage.Spec{ID: "vol-solo", Name: "solo", SizeBytes: 1 << 30, Class: "default", Replication: 3},
		storage.Status{State: storage.StateUnderReplicated, Primary: "n1", Placement: []storage.Replica{{NodeID: "n1", Healthy: true}}},
	)
	for _, path := range []string{"/volumes", "/volumes/solo"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "1 of 3") || !strings.Contains(string(body), "no redundancy") {
			t.Errorf("%s does not say 1 of 3 / no redundancy: %s", path, body)
		}
	}
}

func TestVolumeCreateRejectsBadSize(t *testing.T) {
	srv, pw, _ := newClusterTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)

	resp := postVolumeForm(t, client, csrf, srv.URL+"/volumes", url.Values{
		"name": {"bad"}, "size": {"not-a-size"}, "class": {"default"}, "replication": {"3"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestVolumeDetailShowsReplicaTableAndSnapshots(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	putTestVolume(t, st,
		storage.Spec{ID: "vol-bbbb", Name: "web", SizeBytes: 20 << 30, Class: "default", Replication: 2},
		storage.Status{State: storage.StateDegraded, Primary: "n1", Placement: []storage.Replica{
			{NodeID: "n1", Role: storage.RolePrimary, Healthy: true},
			{NodeID: "n2", Role: storage.RoleResyncing, Healthy: false, SyncPercent: 42},
		}},
	)
	if err := storage.PutSnapshot(context.Background(), st, "vol-bbbb", storage.SnapshotRecord{
		Name: "before-upgrade", Node: "n1", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	resp, err := client.Get(srv.URL + "/volumes/web")
	if err != nil {
		t.Fatalf("GET /volumes/web: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{"n1", "n2", "Resyncing", "42%", "before-upgrade", "Degraded"} {
		if !strings.Contains(text, want) {
			t.Errorf("detail page missing %q: %s", want, text)
		}
	}
}

func TestVolumeResizeQueuesGrowOnlyOp(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)

	putTestVolume(t, st,
		storage.Spec{ID: "vol-cccc", Name: "grow", SizeBytes: 5 << 30, Class: "default", Replication: 1},
		storage.Status{State: storage.StateHealthy, Primary: "n1"},
	)

	// A same-or-smaller size is refused before anything is queued.
	resp := postVolumeForm(t, client, csrf, srv.URL+"/volumes/grow/resize", url.Values{"size": {"5Gi"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("shrink-or-equal status = %d, want 400", resp.StatusCode)
	}

	resp = postVolumeForm(t, client, csrf, srv.URL+"/volumes/grow/resize", url.Values{"size": {"10Gi"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grow status = %d, want 200 (after following redirect)", resp.StatusCode)
	}

	e, err := st.Get(context.Background(), store.Key("/volumes/_ops/resize/vol-cccc"))
	if err != nil {
		t.Fatalf("resize op key missing: %v", err)
	}
	var op struct {
		Target    string `json:"target"`
		SizeBytes uint64 `json:"sizeBytes"`
	}
	if err := json.Unmarshal(e.Value, &op); err != nil {
		t.Fatalf("unmarshal resize op: %v", err)
	}
	if op.Target != "grow" || op.SizeBytes != 10<<30 {
		t.Errorf("resize op = %+v, want target=grow sizeBytes=10Gi", op)
	}
}

func TestVolumeSnapshotQueuesOpAndRefusesDuplicateName(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)

	putTestVolume(t, st,
		storage.Spec{ID: "vol-dddd", Name: "snapme", SizeBytes: 5 << 30, Class: "default", Replication: 1},
		storage.Status{State: storage.StateHealthy, Primary: "n1"},
	)

	resp := postVolumeForm(t, client, csrf, srv.URL+"/volumes/snapme/snapshot", url.Values{"name": {"nightly"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (after following redirect)", resp.StatusCode)
	}
	e, err := st.Get(context.Background(), store.Key("/volumes/_ops/snapshot/vol-dddd"))
	if err != nil {
		t.Fatalf("snapshot op key missing: %v", err)
	}
	var op struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(e.Value, &op); err != nil || op.Name != "nightly" {
		t.Fatalf("snapshot op = %s, want name=nightly (err=%v)", e.Value, err)
	}

	// A name the volume already has recorded (not just queued) is refused up front.
	if err := storage.PutSnapshot(context.Background(), st, "vol-dddd", storage.SnapshotRecord{Name: "nightly", Node: "n1", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	resp = postVolumeForm(t, client, csrf, srv.URL+"/volumes/snapme/snapshot", url.Values{"name": {"nightly"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate snapshot status = %d, want 409", resp.StatusCode)
	}
}

func TestVolumeDeleteQueuesOp(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)

	putTestVolume(t, st,
		storage.Spec{ID: "vol-eeee", Name: "gone", SizeBytes: 5 << 30, Class: "default", Replication: 1},
		storage.Status{State: storage.StateHealthy, Primary: "n1"},
	)

	resp := postVolumeForm(t, client, csrf, srv.URL+"/volumes/gone/delete", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (after following the redirect to /volumes)", resp.StatusCode)
	}
	if _, err := st.Get(context.Background(), store.Key("/volumes/_ops/delete/vol-eeee")); err != nil {
		t.Fatalf("delete op key missing: %v", err)
	}
}

// TestVolumeEventsSSEStreamsLiveUpdate mirrors B1's own
// TestClusterEventsSSEStreamsLiveUpdate: an initial "creating" snapshot,
// then a live update once the leader's controller (simulated here by a
// direct store write) turns the pending request into a real volume.
func TestVolumeEventsSSEStreamsLiveUpdate(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	if _, err := st.Put(context.Background(), storage.PendingCreateKey("live"), nil); err != nil {
		t.Fatalf("Put pending: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/volumes/live/events", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /volumes/live/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	type read struct {
		text string
		err  error
	}
	lines := make(chan read, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				lines <- read{text: string(buf[:n])}
			}
			if err != nil {
				lines <- read{err: err}
				return
			}
		}
	}()

	var got string
	drain := func(want string, timeout time.Duration) {
		t.Helper()
		deadline := time.After(timeout)
		for !strings.Contains(got, want) {
			select {
			case r := <-lines:
				if r.err != nil {
					t.Fatalf("read SSE stream: %v (got so far: %q)", r.err, got)
				}
				got += r.text
			case <-deadline:
				t.Fatalf("%q not seen within %s (got: %q)", want, timeout, got)
			}
		}
	}

	drain("Creating", 10*time.Second)

	putTestVolume(t, st,
		storage.Spec{ID: "vol-live1", Name: "live", SizeBytes: 1 << 30, Class: "default", Replication: 1},
		storage.Status{State: storage.StateHealthy, Primary: "n1"},
	)
	if err := st.Delete(context.Background(), storage.PendingCreateKey("live"), 0); err != nil {
		t.Fatalf("Delete pending: %v", err)
	}

	drain("Healthy", 10*time.Second)
}
