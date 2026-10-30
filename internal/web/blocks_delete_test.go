package web

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// putStorageBlock stores a block "web" with one storage entry and its volume.
func putStorageBlock(t *testing.T, st store.Store) {
	t.Helper()
	one := int32(1)
	raw, err := proto.Marshal(&pb.Block{
		Metadata: &pb.Metadata{Name: "web", Namespace: "default"},
		Spec:     &pb.BlockSpec{Type: "util/echo", Replicas: &one, Storage: []*pb.Storage{{Name: "data", Size: "1Gi"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), "/blocks/default/web", raw); err != nil {
		t.Fatal(err)
	}
	putTestVolume(t, st, storage.Spec{ID: "vol-web", Name: "blk-default-web-data", SizeBytes: 1 << 30, Replication: 1},
		storage.Status{State: storage.StateHealthy, Primary: "n1"})
}

func TestBlockDeleteWithVolumesQueuesTheirDelete(t *testing.T) {
	for _, tc := range []struct {
		form url.Values
		want bool
	}{{url.Values{}, false}, {url.Values{"volumes": {"1"}}, true}} {
		srv, pw, st := newTestServerStore(t)
		client, csrf := loggedInClient(t, srv, pw)
		putStorageBlock(t, st)
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp := postVolumeForm(t, client, csrf, srv.URL+"/blocks/default/web/delete", tc.form)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("delete status = %d, want 303", resp.StatusCode)
		}
		_, err := st.Get(context.Background(), "/volumes/_ops/delete/vol-web")
		if got := err == nil; got != tc.want {
			t.Errorf("form %v: volume delete queued = %v, want %v", tc.form, got, tc.want)
		}
	}
}
