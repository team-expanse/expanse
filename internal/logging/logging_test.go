package logging

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSetup(t *testing.T) {
	var buf bytes.Buffer
	l, err := Setup(Options{Level: "debug", Format: "json", Output: &buf})
	if err != nil {
		t.Fatalf("Setup error: %v", err)
	}
	l.Info("test message")
	out := buf.String()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("invalid JSON output: %s", out)
	}
	if m["msg"] != "test message" {
		t.Errorf("expected msg 'test message', got %v", m["msg"])
	}
}

func TestWithComponent(t *testing.T) {
	var buf bytes.Buffer
	l, err := Setup(Options{Level: "info", Format: "json", Output: &buf})
	if err != nil {
		t.Fatalf("Setup error: %v", err)
	}
	l2 := WithComponent(l, "test")
	l2.Info("test message")
	out := buf.String()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("invalid JSON output: %s", out)
	}
	if m["component"] != "test" {
		t.Errorf("expected component 'test', got %v", m["component"])
	}
}

func TestInvalidLevel(t *testing.T) {
	var buf bytes.Buffer
	_, err := Setup(Options{Level: "invalid", Format: "json", Output: &buf})
	if err == nil {
		t.Error("expected error for invalid level")
	}
}
