package main

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// runEcho with the exact --config arg the agent's spec file carries.
func TestRunEchoConfigBody(t *testing.T) {
	args := []string{"--config", `{"body":"deploy-test\n","port":18091}`}
	errCh := make(chan error, 1)
	go func() { errCh <- runEcho(context.Background(), args) }()
	time.Sleep(300 * time.Millisecond)
	resp, err := http.Get("http://localhost:18091/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "deploy-test\n" {
		t.Fatalf("body = %q, want %q", body, "deploy-test\n")
	}
}
