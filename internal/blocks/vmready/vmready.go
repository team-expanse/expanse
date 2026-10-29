// Package vmready turns what a VM guest's systemd reports over vsock (the vmm.notify_socket
// credential) into whether the guest has booted. Measured by nix/tests/vm-vsock-notify-probe.nix.
package vmready

import (
	"fmt"
	"strings"
)

// ReadyPrefix starts every status text that means ready; the agent reads it back from the unit.
const ReadyPrefix = "ready:"

// Tracker follows one guest's notify messages.
type Tracker struct {
	ready     bool            // READY=1 seen
	active    map[string]bool // targets the guest reported active
	units     bool            // the guest reports units at all (systemd 256 and later)
	stopping  bool            // multi-user.target went inactive after being active
	shutdown  string          // X_SYSTEMD_SHUTDOWN value, once sent
	exitState string          // EXIT_STATUS value, once sent
	resumed   bool            // the guest was restored running from a saved state
}

// NewTracker starts a guest in the booting state.
func NewTracker() *Tracker { return &Tracker{active: map[string]bool{}} }

// ResumedTracker starts a guest restored from a saved state: it was up when saved and won't boot again.
func ResumedTracker() *Tracker { return &Tracker{active: map[string]bool{}, resumed: true} }

// Observe applies one notify datagram: newline-separated KEY=VALUE pairs.
func (t *Tracker) Observe(msg []byte) {
	for _, line := range strings.Split(string(msg), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "READY":
			t.ready = val == "1"
		case "X_SYSTEMD_UNIT_ACTIVE":
			t.active[val] = true
			t.units = true
		case "X_SYSTEMD_UNIT_INACTIVE":
			if val == "multi-user.target" && t.active[val] {
				t.stopping = true
			}
			delete(t.active, val)
		case "X_SYSTEMD_SHUTDOWN":
			t.shutdown = val
		case "EXIT_STATUS":
			t.exitState = val
		}
	}
}

// Ready reports a booted guest: READY=1 and multi-user.target active (emergency mode sends
// READY=1 too). A guest whose systemd reports no units (254, 255) is ready on READY=1 alone.
func (t *Tracker) Ready() bool {
	if t.resumed {
		return t.shutdown == "" && !t.stopping
	}
	return t.ready && (t.active["multi-user.target"] || !t.units)
}

// Status is a one-line description, prefixed with ReadyPrefix exactly when Ready.
func (t *Tracker) Status() string {
	switch {
	case t.shutdown != "":
		return fmt.Sprintf("not ready: guest powered off (exit status %s)", orUnknown(t.exitState))
	case t.stopping:
		return "not ready: guest shutting down"
	case t.resumed:
		return ReadyPrefix + " resumed from its saved state"
	case t.Ready() && !t.units:
		return ReadyPrefix + " guest systemd reported READY"
	case t.Ready():
		return ReadyPrefix + " multi-user.target reached"
	case t.active["emergency.target"]:
		return "not ready: guest in emergency.target"
	case t.active["rescue.target"]:
		return "not ready: guest in rescue.target"
	case t.ready:
		return "not ready: guest started, waiting for multi-user.target" // briefly on every boot; for good under a smaller default target
	}
	return "booting"
}

// StatusTextReady reports whether a unit's status text, as published from Status, means ready.
func StatusTextReady(text string) bool { return strings.HasPrefix(text, ReadyPrefix) }

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
