package web

import (
	"strings"
	"testing"

	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// The YAML view masks every writeOnly value, top-level or nested, on every render.
func TestManifestYAMLHidesSecrets(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{
		"password":{"type":"string","writeOnly":true},
		"user":{"type":"string"},
		"tls":{"type":"object","properties":{"key":{"type":"string","writeOnly":true}}}}}`)
	cfg, err := structpb.NewStruct(map[string]any{"password": "hunter2", "user": "alice", "tls": map[string]any{"key": "PRIVATE"}})
	if err != nil {
		t.Fatal(err)
	}
	b := &pb.Block{Metadata: &pb.Metadata{Name: "db"}, Spec: &pb.BlockSpec{Type: "db/x", Config: cfg}}

	out := manifestYAML(b, schema)
	for _, secret := range []string{"hunter2", "PRIVATE"} {
		if strings.Contains(out, secret) {
			t.Errorf("YAML shows secret %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "alice") || strings.Count(out, "<hidden>") != 2 {
		t.Errorf("YAML should keep plain values and mask both secrets:\n%s", out)
	}
}
