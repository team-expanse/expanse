package web

import (
	"net/http/httptest"
	"strings"
	"testing"

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
