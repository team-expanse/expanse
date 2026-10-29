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
