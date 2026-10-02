package vmready

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeConn struct {
	peer uint32
	msg  string
}

// fakeListener hands out its connections in order, then reports the listener closed.
type fakeListener struct{ conns []fakeConn }

func (l *fakeListener) Accept(ctx context.Context) (io.ReadCloser, uint32, error) {
	if len(l.conns) == 0 {
		return nil, 0, errClosed
	}
	c := l.conns[0]
	l.conns = l.conns[1:]
	return io.NopCloser(strings.NewReader(c.msg)), c.peer, nil
}

var errClosed = errors.New("listener closed")

func serve(t *testing.T, cid uint32, conns []fakeConn) []string {
	t.Helper()
	var published []string
	err := Serve(context.Background(), &fakeListener{conns: conns}, cid, func(s string) { published = append(published, s) })
	if !errors.Is(err, errClosed) {
		t.Fatalf("Serve returned %v, want the listener's error", err)
	}
	return published
}

func TestServePublishesEachChangeOnce(t *testing.T) {
	got := serve(t, 7, []fakeConn{
		{7, "X_SYSTEMD_HOSTNAME=nixos"},
		{7, "READY=1\nSTATUS=Ready."},
		{7, "X_SYSTEMD_UNIT_ACTIVE=multi-user.target"},
		{7, "X_SYSTEMD_UNIT_ACTIVE=getty.target"},
		{7, "X_SYSTEMD_UNIT_INACTIVE=multi-user.target"},
	})
	want := []string{"booting", "not ready: guest started, waiting for multi-user.target",
		"ready: multi-user.target reached", "not ready: guest shutting down"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("published %q, want %q", got, want)
	}
}

func TestServeIgnoresOtherGuests(t *testing.T) {
	got := serve(t, 7, []fakeConn{
		{8, "READY=1"},
		{8, "X_SYSTEMD_UNIT_ACTIVE=multi-user.target"},
	})
	if len(got) != 1 || got[0] != "booting" {
		t.Fatalf("published %q, want only booting", got)
	}
}

func TestCIDForIsStableAndUsable(t *testing.T) {
	a, b := CIDFor("default-web1-0"), CIDFor("default-web1-0")
	if a != b {
		t.Fatalf("CIDFor not stable: %d vs %d", a, b)
	}
	if CIDFor("default-web2-0") == a {
		t.Fatal("two instances share a first-choice CID")
	}
	for _, name := range []string{"", "a", "default-web1-0", strings.Repeat("x", 300)} {
		if c := CIDFor(name); c < MinCID || c > MaxCID {
			t.Errorf("CIDFor(%q) = %d, outside [%d, %d]", name, c, MinCID, MaxCID)
		}
	}
	if NextCID(MaxCID) != MinCID || NextCID(MinCID) != MinCID+1 {
		t.Fatal("NextCID does not wrap within the usable range")
	}
}

// The guest is told to report to the host (CID 2) on a port equal to its own CID.
func TestQEMUArgs(t *testing.T) {
	got := strings.Join(QEMUArgs(1234, 4), " ")
	want := "-device vhost-vsock-pci,guest-cid=1234,vhostfd=4 " +
		"-smbios type=11,value=io.systemd.credential:vmm.notify_socket=vsock-stream:2:1234"
	if got != want {
		t.Fatalf("QEMUArgs = %q\nwant      %q", got, want)
	}
}
