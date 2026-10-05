package main

import (
	"net"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/testsock"
)

func TestInitRefusesWhileTheAgentRuns(t *testing.T) {
	sock := testsock.Path(t, "agent.sock")
	if err := refuseIfAgentRunning(sock); err != nil {
		t.Fatalf("no agent listening, got %v", err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close() //nolint:errcheck
	err = refuseIfAgentRunning(sock)
	if err == nil || !strings.Contains(err.Error(), "systemctl stop expansed") {
		t.Fatalf("agent listening, got %v; want a refusal naming the fix", err)
	}
}

func TestEmitWithoutATableFallsBackToYAML(t *testing.T) {
	if err := emit(&ctlOpts{output: "table"}, nil, map[string]string{"phase": "PENDING"}); err != nil {
		t.Fatalf("emit with no table and default output: %v", err)
	}
}
