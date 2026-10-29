package qmp

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const banner = `{"QMP":{"version":{"qemu":{"micro":1,"minor":1,"major":11},"package":""},"capabilities":["oob"]}}`

// fakeQEMU answers the handshake, then each command from replies (default: an empty return).
// It sends an event before each later reply, as QEMU may, and records the commands it got.
func fakeQEMU(t *testing.T, replies map[string]string) (string, <-chan []string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "qmp.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got := make(chan []string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var cmds []string
		defer func() { got <- cmds }()
		fmt.Fprintln(c, banner)
		dec := json.NewDecoder(c) // QMP commands need no newline between them
		for {
			var cmd struct{ Execute string }
			if err := dec.Decode(&cmd); err != nil {
				return
			}
			cmds = append(cmds, cmd.Execute)
			if cmd.Execute != "qmp_capabilities" { // QEMU sends no events before the handshake
				fmt.Fprintln(c, `{"event":"RTC_CHANGE","data":{"offset":0},"timestamp":{"seconds":1,"microseconds":0}}`)
			}
			reply, ok := replies[cmd.Execute]
			if !ok {
				reply = `{"return":{}}`
			}
			fmt.Fprintln(c, reply)
		}
	}()
	return sock, got
}

func TestExecuteDecodesTheReturnValue(t *testing.T) {
	sock, got := fakeQEMU(t, map[string]string{
		"query-status": `{"return":{"status":"running","running":true}}`,
	})
	m, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	var st struct{ Status string }
	if err := m.Execute("query-status", nil, &st); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if st.Status != "running" {
		t.Errorf("status = %q, want running", st.Status)
	}
	if cmds := <-got; !slices.Equal(cmds, []string{"qmp_capabilities", "query-status"}) {
		t.Errorf("commands = %q", cmds)
	}
}

func TestExecuteReturnsQEMUsError(t *testing.T) {
	sock, _ := fakeQEMU(t, map[string]string{
		"cont": `{"error":{"class":"GenericError","desc":"migration is in progress"}}`,
	})
	m, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	err = m.Execute("cont", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "migration is in progress") {
		t.Errorf("err = %v, want QEMU's own message", err)
	}
}

func TestPowerdownAsksTheGuestToShutDown(t *testing.T) {
	sock, got := fakeQEMU(t, nil)
	if err := Powerdown(sock); err != nil {
		t.Fatal(err)
	}
	if cmds := <-got; !slices.Equal(cmds, []string{"qmp_capabilities", "system_powerdown"}) {
		t.Errorf("commands = %q", cmds)
	}
}

func TestPowerdownFailsWithoutQEMU(t *testing.T) {
	if err := Powerdown(filepath.Join(t.TempDir(), "none.sock")); err == nil {
		t.Error("powerdown with no monitor succeeded")
	}
}

func TestSocketPathIsUnderRun(t *testing.T) {
	if p := SocketPath("default-vm-0"); p != "/run/expblk-default-vm-0-qmp.sock" {
		t.Errorf("SocketPath = %q", p)
	}
}
