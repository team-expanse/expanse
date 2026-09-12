package version

import (
	"encoding/json"
	"testing"
)

func TestGet(t *testing.T) {
	info := Get()
	if info.Version == "" {
		t.Error("Version should not be empty")
	}
	if info.Commit == "" {
		t.Error("Commit should not be empty")
	}
	if info.Platform == "" {
		t.Error("Platform should not be empty")
	}
	if info.GoVersion == "" {
		t.Error("GoVersion should not be empty")
	}
}

func TestString(t *testing.T) {
	info := Info{Version: "1.0.0", Commit: "abc123", Platform: "linux/amd64", GoVersion: "go1.23"}
	s := info.String()
	if s != "expanse 1.0.0 (abc123) linux/amd64 go1.23" {
		t.Errorf("unexpected string: %q", s)
	}
}

func TestJSONMarshal(t *testing.T) {
	info := Info{Version: "1.0.0", Commit: "abc123", Date: "2024-01-01", Platform: "linux/amd64", GoVersion: "go1.23"}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var out Info
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if out.Version != "1.0.0" {
		t.Errorf("round-trip failed: got %v", out)
	}
}