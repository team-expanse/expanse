package qga

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type command struct {
	Execute   string
	Arguments json.RawMessage
}

// serveAgent answers each connection's sync and one command with handle's reply, the way qemu-ga does.
func serveAgent(t *testing.T, handle func(command) string) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "qga.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(c)
			var sync, cmd command
			_, _ = r.ReadByte()
			line, _ := r.ReadBytes('\n')
			_ = json.Unmarshal(line, &sync)
			var id struct{ ID int64 }
			_ = json.Unmarshal(sync.Arguments, &id)
			fmt.Fprintf(c, "\xff{\"return\": %d}\n", id.ID)
			line, _ = r.ReadBytes('\n')
			_ = json.Unmarshal(line, &cmd)
			fmt.Fprint(c, handle(cmd))
			c.Close()
		}
	}()
	return sock
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestExecRunsTheCommandAndWaitsForItsExit(t *testing.T) {
	var polls atomic.Int32
	var started command
	sock := serveAgent(t, func(c command) string {
		switch c.Execute {
		case "guest-exec":
			started = c
			return `{"return":{"pid":42}}`
		case "guest-exec-status":
			if !strings.Contains(string(c.Arguments), `"pid":42`) {
				return `{"error":{"class":"GenericError","desc":"wrong pid"}}`
			}
			if polls.Add(1) < 3 {
				return `{"return":{"exited":false}}`
			}
			return fmt.Sprintf(`{"return":{"exited":true,"exitcode":3,"out-data":%q,"err-data":%q}}`, b64("hi\n"), b64("oops"))
		}
		return `{"error":{"class":"CommandNotFound","desc":"` + c.Execute + `"}}`
	})
	res, err := Exec(context.Background(), sock, []string{"sh", "-c", "cat; exit 3"}, []byte("in"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || string(res.Stdout) != "hi\n" || string(res.Stderr) != "oops" {
		t.Errorf("Exec = %+v", res)
	}
	var args struct {
		Path    string
		Arg     []string
		Input   string `json:"input-data"`
		Capture bool   `json:"capture-output"`
	}
	if err := json.Unmarshal(started.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args.Path != "sh" || strings.Join(args.Arg, " ") != "-c cat; exit 3" || args.Input != b64("in") || !args.Capture {
		t.Errorf("guest-exec arguments = %s", started.Arguments)
	}
}

func TestExecReportsAKillingSignalAsAShellWould(t *testing.T) {
	sock := serveAgent(t, func(c command) string {
		if c.Execute == "guest-exec" {
			return `{"return":{"pid":7}}`
		}
		return `{"return":{"exited":true,"signal":9}}`
	})
	res, err := Exec(context.Background(), sock, []string{"sleep", "60"}, nil)
	if err != nil || res.ExitCode != 128+9 {
		t.Errorf("Exec = %+v, %v; want exit code 137", res, err)
	}
}

func TestExecReportsTheAgentRefusingTheCommand(t *testing.T) {
	sock := serveAgent(t, func(command) string {
		return `{"error":{"class":"GenericError","desc":"Guest agent command failed, error was 'Failed to execute child process “nope” (No such file or directory)'"}}`
	})
	if _, err := Exec(context.Background(), sock, []string{"nope"}, nil); err == nil || !strings.Contains(err.Error(), "No such file") {
		t.Errorf("Exec error = %v, want the agent's reason", err)
	}
}

func TestSocketPathIsOutsideTheUnitsPrivateTmp(t *testing.T) {
	if got := SocketPath("default-vm1-0"); got != "/run/expblk-default-vm1-0-qga.sock" {
		t.Errorf("SocketPath = %q", got)
	}
}
