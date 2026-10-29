package qga

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	gosync "sync"
	"testing"
)

// agentByCommand serves any number of connections, answering each command from replies and
// recording it as "cmd {args}".
func agentByCommand(t *testing.T, replies map[string]string) (string, func() []string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "qga.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var mu gosync.Mutex
	var got []string
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(c)
			_, _ = r.ReadByte()
			var req struct {
				Execute   string
				Arguments json.RawMessage
			}
			line, _ := r.ReadBytes('\n')
			_ = json.Unmarshal(line, &req)
			var id struct{ ID int64 }
			_ = json.Unmarshal(req.Arguments, &id)
			fmt.Fprintf(c, "\xff{\"return\": %d}\n", id.ID)
			line, _ = r.ReadBytes('\n')
			req.Arguments = nil
			_ = json.Unmarshal(line, &req)
			mu.Lock()
			got = append(got, strings.TrimSpace(req.Execute+" "+string(req.Arguments)))
			mu.Unlock()
			fmt.Fprintln(c, replies[req.Execute])
			c.Close()
		}
	}()
	return sock, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(got) }
}

func TestOnlineBringsUpOnlyWhatIsOffline(t *testing.T) {
	sock, calls := agentByCommand(t, map[string]string{
		"guest-get-vcpus":         `{"return":[{"logical-id":0,"online":true,"can-offline":false},{"logical-id":1,"online":false,"can-offline":true}]}`,
		"guest-set-vcpus":         `{"return":1}`,
		"guest-get-memory-blocks": `{"return":[{"phys-index":8,"online":true,"can-offline":true},{"phys-index":9,"online":false,"can-offline":true}]}`,
		"guest-set-memory-blocks": `{"return":[{"phys-index":9,"response":"success"}]}`,
	})
	if err := Online(context.Background(), sock); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"guest-get-vcpus",
		`guest-set-vcpus {"vcpus":[{"logical-id":1,"online":true}]}`,
		"guest-get-memory-blocks",
		`guest-set-memory-blocks {"mem-blks":[{"phys-index":9,"online":true}]}`,
	}
	if got := calls(); !slices.Equal(got, want) {
		t.Errorf("calls = %q\nwant %q", got, want)
	}
}

func TestOnlineSendsNothingWhenAllIsUp(t *testing.T) {
	sock, calls := agentByCommand(t, map[string]string{
		"guest-get-vcpus":         `{"return":[{"logical-id":0,"online":true}]}`,
		"guest-get-memory-blocks": `{"return":[{"phys-index":8,"online":true}]}`,
	})
	if err := Online(context.Background(), sock); err != nil {
		t.Fatal(err)
	}
	if got := calls(); !slices.Equal(got, []string{"guest-get-vcpus", "guest-get-memory-blocks"}) {
		t.Errorf("calls = %q", got)
	}
}
