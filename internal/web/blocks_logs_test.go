package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/store/boltstore"
	pb "github.com/expanse/expanse/proto"
)

type htmlLogBlocks struct{ previewBlocks }

func (htmlLogBlocks) StreamLogs(_ *pb.LogsRequest, s pb.BlockService_StreamLogsServer) error {
	return s.Send(&pb.LogLine{Line: `<div hx-post="/blocks/default/x/delete" hx-trigger="load">a & b</div>`})
}

// htmx swaps log lines in as HTML, so a workload's output must arrive as text.
func TestBlockLogsStreamEscapesHTML(t *testing.T) {
	s := &Server{blocks: htmlLogBlocks{}}
	rec := httptest.NewRecorder()
	s.handleBlockLogsStream(rec, httptest.NewRequest("GET", "/blocks/default/x/logs/stream", nil))

	body := rec.Body.String()
	if strings.Contains(body, "<div") {
		t.Fatalf("log line sent as markup: %q", body)
	}
	if want := "&lt;div hx-post=&#34;/blocks/default/x/delete&#34; hx-trigger=&#34;load&#34;&gt;a &amp; b&lt;/div&gt;"; !strings.Contains(body, want) {
		t.Errorf("body = %q, want the escaped line %q", body, want)
	}
}

type deletedBlocks struct{ previewBlocks }

func (deletedBlocks) Watch(_ *pb.WatchBlocksRequest, s pb.BlockService_WatchServer) error {
	b := &pb.Block{Metadata: &pb.Metadata{Namespace: "default", Name: "x"}, Spec: &pb.BlockSpec{Type: "util/echo"}}
	for _, ev := range []*pb.BlockEvent{
		{Type: pb.EventType_EVENT_MODIFIED, Block: b},
		{Type: pb.EventType_EVENT_MODIFIED, Block: b}, // a probe heartbeat: nothing shown changed
		{Type: pb.EventType_EVENT_DELETED},
	} {
		if err := s.Send(ev); err != nil {
			return err
		}
	}
	return nil
}

func TestBlockEventsSkipRepeatsAndShowDeletion(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New("n1", st, deletedBlocks{}, nil, nil, clusterSecretForTest)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/blocks/default/x/events", nil)
	req.SetPathValue("ns", "default")
	req.SetPathValue("name", "x")
	rec := httptest.NewRecorder()
	s.handleBlockEvents(rec, req)

	body := rec.Body.String()
	if n := strings.Count(body, "event: block"); n != 2 {
		t.Errorf("sent %d block events, want 2 (the block, then its deletion): %q", n, body)
	}
	if !strings.Contains(body, "This block was deleted") {
		t.Errorf("deletion not shown: %q", body)
	}
}

type failingBlocks struct{ previewBlocks }

func (failingBlocks) List(context.Context, *pb.ListBlocksRequest) (*pb.ListBlocksResponse, error) {
	return nil, fmt.Errorf("store unavailable")
}

// A full-page load that fails renders the styled error page, not bare text.
func TestBlocksListErrorIsAStyledPage(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New("n1", st, failingBlocks{}, nil, nil, clusterSecretForTest)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleBlocksList(rec, httptest.NewRequest("GET", "/blocks", nil))
	if body := rec.Body.String(); !strings.Contains(body, "Served by node") || !strings.Contains(body, "store unavailable") {
		t.Errorf("blocks list error is not the styled page: %q", body)
	}
}
