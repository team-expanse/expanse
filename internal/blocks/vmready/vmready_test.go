package vmready

import "testing"

// Message sequences as measured by nix/tests/vm-vsock-notify-probe.nix (systemd 260.2).
var (
	bootMsgs = []string{
		"X_SYSTEMD_MACHINE_ID=bc7b125b9d8d47ffab31cefb22679654",
		"X_SYSTEMD_HOSTNAME=nixos",
		"X_SYSTEMD_UNIT_ACTIVE=sysinit.target",
		"X_SYSTEMD_UNIT_ACTIVE=basic.target",
		"READY=1\nSTATUS=Ready.",
		"X_SYSTEMD_UNIT_ACTIVE=multi-user.target",
	}
	emergencyMsgs = []string{
		"X_SYSTEMD_HOSTNAME=nixos",
		"X_SYSTEMD_UNIT_ACTIVE=emergency.target",
		"READY=1\nSTATUS=Ready.",
	}
	shutdownMsgs = []string{
		"X_SYSTEMD_UNIT_INACTIVE=multi-user.target",
		"X_SYSTEMD_UNIT_ACTIVE=shutdown.target",
		"EXIT_STATUS=0\nX_SYSTEMD_SHUTDOWN=poweroff",
	}
)

func feed(t *testing.T, tr *Tracker, msgs []string) {
	t.Helper()
	for _, m := range msgs {
		tr.Observe([]byte(m))
	}
}

func TestANewGuestIsBooting(t *testing.T) {
	tr := NewTracker()
	if tr.Ready() {
		t.Fatal("ready before any message")
	}
	if got := tr.Status(); got != "booting" {
		t.Fatalf("status = %q, want booting", got)
	}
}

func TestReadyNeedsBothReadyAndMultiUser(t *testing.T) {
	tr := NewTracker()
	feed(t, tr, bootMsgs[:5]) // READY=1, but multi-user.target not yet active
	if tr.Ready() {
		t.Fatal("ready before multi-user.target")
	}
	tr.Observe([]byte(bootMsgs[5]))
	if !tr.Ready() {
		t.Fatalf("not ready after a full boot; status %q", tr.Status())
	}
	if got := tr.Status(); got != "ready: multi-user.target reached" {
		t.Fatalf("status = %q", got)
	}
}

func TestMultiUserBeforeReadyIsNotReady(t *testing.T) {
	tr := NewTracker()
	tr.Observe([]byte("X_SYSTEMD_UNIT_ACTIVE=multi-user.target"))
	if tr.Ready() {
		t.Fatal("ready without READY=1")
	}
}

func TestEmergencyModeIsNotReady(t *testing.T) {
	tr := NewTracker()
	feed(t, tr, emergencyMsgs)
	if tr.Ready() {
		t.Fatal("emergency mode counted as ready")
	}
	if got := tr.Status(); got != "not ready: guest in emergency.target" {
		t.Fatalf("status = %q", got)
	}
}

func TestRescueModeIsNotReady(t *testing.T) {
	tr := NewTracker()
	feed(t, tr, []string{"X_SYSTEMD_UNIT_ACTIVE=rescue.target", "READY=1"})
	if tr.Ready() || tr.Status() != "not ready: guest in rescue.target" {
		t.Fatalf("ready=%v status=%q", tr.Ready(), tr.Status())
	}
}

func TestShutdownTakesReadinessAway(t *testing.T) {
	tr := NewTracker()
	feed(t, tr, bootMsgs)
	tr.Observe([]byte(shutdownMsgs[0]))
	if tr.Ready() {
		t.Fatal("still ready after multi-user.target stopped")
	}
	if got := tr.Status(); got != "not ready: guest shutting down" {
		t.Fatalf("status = %q", got)
	}
	feed(t, tr, shutdownMsgs[1:])
	if got := tr.Status(); got != "not ready: guest powered off (exit status 0)" {
		t.Fatalf("status = %q", got)
	}
}

func TestStatusTextReady(t *testing.T) {
	for text, want := range map[string]bool{
		"ready: multi-user.target reached":        true,
		"ready: not waited on (guestReady: none)": true,
		"booting":                              false,
		"not ready: guest in emergency.target": false,
		"":                                     false,
	} {
		if got := StatusTextReady(text); got != want {
			t.Errorf("StatusTextReady(%q) = %v, want %v", text, got, want)
		}
	}
}

// A guest booted to a smaller target (systemd.unit=basic.target) finishes starting but never serves.
func TestStartedWithoutMultiUserIsNotReady(t *testing.T) {
	tr := NewTracker()
	feed(t, tr, []string{"X_SYSTEMD_UNIT_ACTIVE=basic.target", "READY=1\nSTATUS=Ready."})
	if tr.Ready() {
		t.Fatal("ready without multi-user.target")
	}
	if got := tr.Status(); got != "not ready: guest started, waiting for multi-user.target" {
		t.Fatalf("status = %q", got)
	}
}

// Leaving emergency mode (sulogin reads EOF from the console and continues) is booting again.
func TestLeavingEmergencyModeIsBooting(t *testing.T) {
	tr := NewTracker()
	feed(t, tr, []string{"X_SYSTEMD_UNIT_ACTIVE=emergency.target", "X_SYSTEMD_UNIT_INACTIVE=emergency.target"})
	if got := tr.Status(); got != "booting" {
		t.Fatalf("status = %q", got)
	}
}
