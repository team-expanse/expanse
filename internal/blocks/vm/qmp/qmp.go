// Package qmp drives a VM's QEMU process over its QMP monitor socket.
package qmp

import (
	"encoding/json"
	"fmt"
	"time"

	dqmp "github.com/digitalocean/go-qemu/qmp"
)

// SocketPath is the monitor socket for a VM replica, in /run so the node's root can reach it.
func SocketPath(instance string) string {
	return "/run/expblk-" + instance + "-qmp.sock"
}

// dialTimeout bounds connecting to the socket; QEMU answers its handshake at once.
const dialTimeout = 2 * time.Second

// Monitor is one QMP connection; commands on it run one at a time.
type Monitor struct{ m *dqmp.SocketMonitor }

// Dial connects to sock and completes QMP's capabilities handshake.
func Dial(sock string) (*Monitor, error) {
	m, err := dqmp.NewSocketMonitor("unix", sock, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("qmp: %w", err)
	}
	if err := m.Connect(); err != nil {
		_ = m.Disconnect()
		return nil, fmt.Errorf("qmp handshake: %w", err)
	}
	return &Monitor{m}, nil
}

// Close ends the connection.
func (m *Monitor) Close() error { return m.m.Disconnect() }

// Execute runs cmd with args and decodes its return value into out, when out is non-nil.
func (m *Monitor) Execute(cmd string, args, out any) error {
	req, err := json.Marshal(dqmp.Command{Execute: cmd, Args: args})
	if err != nil {
		return err
	}
	raw, err := m.m.Run(req)
	if err != nil {
		return fmt.Errorf("qmp %s: %w", cmd, err)
	}
	if out == nil {
		return nil
	}
	var reply struct{ Return json.RawMessage }
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("qmp %s: %w", cmd, err)
	}
	return json.Unmarshal(reply.Return, out)
}

// Powerdown asks the guest to shut down, as pressing a power button would.
func Powerdown(sock string) error {
	m, err := Dial(sock)
	if err != nil {
		return err
	}
	defer m.Close()
	return m.Execute("system_powerdown", nil, nil)
}

// migratePoll is how often a save or load checks its progress.
const migratePoll = 200 * time.Millisecond

// DialRetry dials sock until it answers or timeout passes, for a QEMU that is still starting.
func DialRetry(sock string, timeout time.Duration) (*Monitor, error) {
	deadline := time.Now().Add(timeout)
	for {
		m, err := Dial(sock)
		if err == nil || time.Now().After(deadline) {
			return m, err
		}
		time.Sleep(migratePoll)
	}
}

// WaitMigration polls until the current migration, outgoing or incoming, completes.
func (m *Monitor) WaitMigration(timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var st struct {
			Status    string `json:"status"`
			ErrorDesc string `json:"error-desc"`
		}
		if err := m.Execute("query-migrate", nil, &st); err != nil {
			return err
		}
		switch st.Status {
		case "completed":
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("qmp: migration %s: %s", st.Status, st.ErrorDesc)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("qmp: migration still %q after %v", st.Status, timeout)
		}
		time.Sleep(poll)
	}
}

// Save pauses the guest and writes its state to path, leaving QEMU paused for a quit.
// On failure the guest runs again.
func Save(sock, path string, timeout time.Duration) error {
	m, err := Dial(sock)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Execute("stop", nil, nil); err != nil {
		return err
	}
	err = m.Execute("migrate", map[string]any{"uri": "file:" + path}, nil)
	if err == nil {
		err = m.WaitMigration(timeout, migratePoll)
	}
	if err != nil {
		_ = m.Execute("cont", nil, nil)
	}
	return err
}

// Load reads a state Save wrote into a QEMU started with -S -incoming defer, then runs the guest.
func Load(sock, path string, timeout time.Duration) error {
	m, err := DialRetry(sock, 10*time.Second)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Execute("migrate-incoming", map[string]any{"uri": "file:" + path}, nil); err != nil {
		return err
	}
	if err := m.WaitMigration(timeout, migratePoll); err != nil {
		return err
	}
	return m.Execute("cont", nil, nil)
}

// Cont runs a paused guest again.
func Cont(sock string) error {
	m, err := Dial(sock)
	if err != nil {
		return err
	}
	defer m.Close()
	return m.Execute("cont", nil, nil)
}

// Quit ends QEMU; its reply may never come, as it exits.
func Quit(sock string) {
	if m, err := Dial(sock); err == nil {
		_ = m.Execute("quit", nil, nil)
		_ = m.Close()
	}
}
