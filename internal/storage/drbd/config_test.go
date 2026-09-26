package drbd

import (
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
)

var update = flag.Bool("update", false, "rewrite golden config files")

func members(n int) []Member {
	ms := make([]Member, n)
	for i := range ms {
		ms[i] = Member{
			Host:    fmt.Sprintf("n%d", i+1),
			NodeID:  i,
			Address: netip.MustParseAddr(fmt.Sprintf("192.168.1.%d", i+1)),
		}
	}
	return ms
}

func resource(n int) Resource {
	return Resource{
		Name:          "vol-a1",
		Minor:         3,
		Port:          7793,
		Disk:          "/dev/vg0/vol-a1",
		SplitBrainCmd: "/run/current-system/sw/bin/expanse-drbd-event",
		Members:       members(n),
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "test", "fixtures", "drbd", "config", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs from golden (rerun with -update to accept):\n%s", name, got)
	}
}

func TestRenderGolden(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5} {
		t.Run(fmt.Sprintf("%d-replicas", n), func(t *testing.T) {
			got, err := resource(n).Render()
			if err != nil {
				t.Fatal(err)
			}
			golden(t, fmt.Sprintf("%d-replicas.res", n), got)
		})
	}
}

func TestRenderIsIndependentOfMemberOrder(t *testing.T) {
	r := resource(3)
	want, _ := r.Render()
	r.Members[0], r.Members[2] = r.Members[2], r.Members[0]
	got, err := r.Render()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("member order changed the output:\n%s\nvs\n%s", got, want)
	}
}

func TestRenderNeverAutoResolvesSplitBrain(t *testing.T) {
	got, err := resource(3).Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, need := range []string{
		"after-sb-0pri disconnect;", "after-sb-1pri disconnect;",
		"after-sb-2pri disconnect;", "rr-conflict disconnect;",
		`split-brain "/run/current-system/sw/bin/expanse-drbd-event";`,
	} {
		if !strings.Contains(got, need) {
			t.Errorf("missing %q in:\n%s", need, got)
		}
	}
	for _, banned := range []string{"discard-", "consensus", "violently", "call-pri-lost", "auto-discard"} {
		if strings.Contains(got, banned) {
			t.Errorf("config contains destructive policy %q:\n%s", banned, got)
		}
	}
}

func TestRenderOmitsHandlerWhenUnset(t *testing.T) {
	r := resource(3)
	r.SplitBrainCmd = ""
	got, err := r.Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "handlers") {
		t.Errorf("handlers block emitted without a command:\n%s", got)
	}
}

// An open must never promote: only the lease-gated promoter may make a node Primary.
func TestRenderDisablesAutoPromote(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5} {
		got, err := resource(n).Render()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "auto-promote no;") {
			t.Errorf("%d replicas: auto-promote not disabled in:\n%s", n, got)
		}
	}
}

func TestRenderQuorumNeedsThreeReplicas(t *testing.T) {
	for n, want := range map[int]string{1: "quorum off;", 2: "quorum off;", 3: "quorum majority;", 5: "quorum majority;"} {
		got, err := resource(n).Render()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, want) {
			t.Errorf("%d replicas: want %q in:\n%s", n, want, got)
		}
	}
}

func TestRenderIPv6Address(t *testing.T) {
	r := resource(2)
	r.Members[1].Address = netip.MustParseAddr("fd00::2")
	got, err := r.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "address ipv6 [fd00::2]:7793;") {
		t.Errorf("ipv6 address not rendered:\n%s", got)
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	cases := map[string]func(*Resource){
		"empty name":           func(r *Resource) { r.Name = "" },
		"option-like name":     func(r *Resource) { r.Name = "-r0" },
		"negative minor":       func(r *Resource) { r.Minor = -1 },
		"minor too large":      func(r *Resource) { r.Minor = 1 << 20 },
		"privileged port":      func(r *Resource) { r.Port = 80 },
		"port too large":       func(r *Resource) { r.Port = 70000 },
		"relative disk":        func(r *Resource) { r.Disk = "vg0/lv" },
		"disk with newline":    func(r *Resource) { r.Disk = "/dev/vg0/a\n}" },
		"no members":           func(r *Resource) { r.Members = nil },
		"duplicate node-id":    func(r *Resource) { r.Members[1].NodeID = 0 },
		"node-id above 31":     func(r *Resource) { r.Members[1].NodeID = 32 },
		"negative node-id":     func(r *Resource) { r.Members[1].NodeID = -1 },
		"duplicate host":       func(r *Resource) { r.Members[1].Host = "n1" },
		"host with brace":      func(r *Resource) { r.Members[1].Host = "n2 {" },
		"empty host":           func(r *Resource) { r.Members[1].Host = "" },
		"unset address":        func(r *Resource) { r.Members[1].Address = netip.Addr{} },
		"handler with quote":   func(r *Resource) { r.SplitBrainCmd = `/bin/x "; rm -rf /` },
		"handler with newline": func(r *Resource) { r.SplitBrainCmd = "/bin/x\n}" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := resource(3)
			mutate(&r)
			_, err := r.Render()
			if experrors.KindOf(err) != experrors.KindInvalid {
				t.Errorf("want KindInvalid, got %v", err)
			}
		})
	}
}

// The default 250 KiB/s floor stalled a full resync under a busy writer; 4 MiB/s finished it
// in 33s at a 3% writer cost (vol-resync-rate probe, ARCHITECTURE A51).
func TestRenderRaisesTheResyncFloor(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5} {
		got, err := resource(n).Render()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "  disk {\n    c-min-rate 4M;\n  }\n") {
			t.Errorf("%d replicas: c-min-rate 4M missing in:\n%s", n, got)
		}
	}
}
