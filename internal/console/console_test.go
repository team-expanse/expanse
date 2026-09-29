package console

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/tuikit"
)

const mdstatMixed = `Personalities : [raid1] [raid0]
md126 : active raid1 vdb2[1] vda2[0]
      20951040 blocks super 1.2 [2/2] [UU]

md127 : active raid1 vda1[0]
      1046528 blocks super 1.0 [2/1] [U_]
      [==>..................]  recovery = 12.5% (131072/1046528) finish=0.1min speed=131072K/sec

unused devices: <none>
`

func TestParseMdstat(t *testing.T) {
	got := parseMdstat(mdstatMixed)
	if len(got) != 2 {
		t.Fatalf("arrays: %+v", got)
	}
	if got[0] != (MDArray{Name: "md126", Level: "raid1", Members: "[UU]", Total: 2, Active: 2}) {
		t.Errorf("md126 = %+v", got[0])
	}
	if got[1].Name != "md127" || got[1].Members != "[U_]" || !got[1].Degraded() || got[1].Sync != "recovery 12.5%" {
		t.Errorf("md127 = %+v", got[1])
	}
	if len(parseMdstat("Personalities :\nunused devices: <none>\n")) != 0 {
		t.Error("no arrays should parse as none")
	}
}

func TestParseProcFiles(t *testing.T) {
	cpu := parseCPUInfo("processor\t: 0\nmodel name\t: Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz\nprocessor\t: 1\nmodel name\t: Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz\n")
	if cpu.Model != "Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz" || cpu.Threads != 2 {
		t.Errorf("cpu = %+v", cpu)
	}
	mem := parseMemInfo("MemTotal:       16303672 kB\nMemFree:         1000 kB\nMemAvailable:   12000000 kB\n")
	if mem.Total != 16303672*1024 || mem.Available != 12000000*1024 {
		t.Errorf("mem = %+v", mem)
	}
	if up := parseUptime("93784.52 180000.00\n"); up != 93784*time.Second {
		t.Errorf("uptime = %v", up)
	}
}

func TestHumanSizes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1536: "1.5 KiB", 16303672 * 1024: "15.5 GiB", 2 << 40: "2.0 TiB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
	if got := humanDuration(93784 * time.Second); got != "1d 2h 3m" {
		t.Errorf("humanDuration = %q", got)
	}
	if got := humanDuration(59 * time.Second); got != "0m" {
		t.Errorf("humanDuration short = %q", got)
	}
}

func healthyInfo() Info {
	return Info{
		Version: "1.1.5", Hostname: "node-a", NodeID: "0f3c1a2b-9c1d-4e2f-8a3b-5c6d7e8f9a0b",
		Uptime: 93784 * time.Second, Now: time.Date(2026, 9, 28, 14, 32, 7, 0, time.UTC),
		Addrs:       []Addr{{Iface: "eth0", IP: "192.168.1.10"}, {Iface: "eth0", IP: "fd00::a"}},
		Agent:       AgentState{Up: true},
		Health:      Health{Overall: "healthy"},
		Cluster:     &Cluster{Name: "lab", Role: "leader", QuorumHave: 3, QuorumNeed: 2, Nodes: 3},
		CPU:         CPU{Model: "Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz", Threads: 8},
		Memory:      Memory{Total: 16 << 30, Available: 12 << 30},
		Disks:       []Disk{{Name: "vda", Size: 32 << 30, Model: "QEMU HARDDISK"}, {Name: "vdb", Size: 32 << 30, Model: "QEMU HARDDISK"}},
		Arrays:      []MDArray{{Name: "md126", Label: "system", Level: "raid1", Members: "[UU]", Total: 2, Active: 2}},
		Filesystems: []Filesystem{{Mount: "/persist", Total: 20 << 30, Free: 15 << 30}},
	}
}

// render checks the frame's shape at sz and returns the plain text.
func render(t *testing.T, in Info, sz tuikit.Size) string {
	t.Helper()
	lines := Render(in, sz, tuikit.Unicode)
	if len(lines) != sz.Rows {
		t.Fatalf("%v: %d lines, want %d", sz, len(lines), sz.Rows)
	}
	for i, l := range lines {
		if w := tuikit.Width(l); w > sz.Cols {
			t.Fatalf("%v: line %d is %d wide: %q", sz, i, w, l)
		}
	}
	return tuikit.Strip(strings.Join(lines, "\n"))
}

func TestRenderHealthyClusteredNode(t *testing.T) {
	for _, sz := range []tuikit.Size{tuikit.VT, {Cols: 160, Rows: 50}} {
		got := render(t, healthyInfo(), sz)
		for _, want := range []string{
			"EXPANSE 1.1.5", "node-a", "192.168.1.10", "fd00::a",
			"https://192.168.1.10:8443", "lab", "leader", "3/2", "HEALTHY", "Xeon", "8 threads", "12.0 GiB free of 16.0 GiB",
			"system  md126 raid1 [UU]", "vda", "QEMU HARDDISK", "1d 2h 3m", "Alt+F2 for a login shell", "14:32:07",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("%v lacks %q:\n%s", sz, want, got)
			}
		}
	}
}

func TestRenderNotClusteredAndAgentDown(t *testing.T) {
	in := healthyInfo()
	in.Cluster = nil
	got := render(t, in, tuikit.VT)
	for _, want := range []string{"not in a cluster yet", "with the agent stopped", "systemctl stop expansed && expanse cluster init --expect 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lacks %q:\n%s", want, got)
		}
	}
	in.Agent = AgentState{Up: false, Err: "agent socket /run/expanse/agent.sock not available"}
	in.Health = Health{}
	got = render(t, in, tuikit.VT)
	for _, want := range []string{"agent not running", "systemctl status expansed", "node-a", "192.168.1.10"} {
		if !strings.Contains(got, want) {
			t.Fatalf("agent-down screen lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "HEALTHY") {
		t.Fatalf("agent-down screen claims health:\n%s", got)
	}
	in.Agent = AgentState{Up: true, Err: "context deadline exceeded"}
	got = render(t, in, tuikit.VT)
	if strings.Contains(got, "not running") || !strings.Contains(got, "agent slow to answer") {
		t.Fatalf("a slow agent must not read as down:\n%s", got)
	}
	in.Agent = AgentState{Up: true}
	if got = render(t, in, tuikit.VT); !strings.Contains(got, "first heartbeat") {
		t.Fatalf("no heartbeat yet should say so:\n%s", got)
	}
}

func TestRenderDegradedMirrorAndFailingChecks(t *testing.T) {
	in := healthyInfo()
	in.Arrays = []MDArray{{Name: "md126", Level: "raid1", Members: "[U_]", Total: 2, Active: 1, Sync: "recovery 12.5%"}}
	in.Health = Health{Overall: "degraded", NoQuorum: true, Failing: []string{"system-mirror", "clock-sync"}}
	in.Cluster.QuorumHave = 1
	got := render(t, in, tuikit.VT)
	for _, want := range []string{"[U_]", "DEGRADED", "recovery 12.5%", "system-mirror", "clock-sync", "1/2", "NO LEADER"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lacks %q:\n%s", want, got)
		}
	}
}

func TestRenderLongHostnameAndManyAddresses(t *testing.T) {
	in := healthyInfo()
	in.Hostname = strings.Repeat("very-long-hostname-", 8)
	for i := 0; i < 40; i++ {
		in.Addrs = append(in.Addrs, Addr{Iface: fmt.Sprintf("wg%d", i), IP: fmt.Sprintf("10.9.%d.1", i)})
	}
	got := render(t, in, tuikit.VT)
	if !strings.Contains(got, "more") {
		t.Fatalf("40 addresses should be summarised:\n%s", got)
	}
	if !strings.Contains(got, "Alt+F2") {
		t.Fatalf("the footer was pushed off the screen:\n%s", got)
	}
}

func TestRenderEmptyInfoDoesNotPanic(t *testing.T) {
	got := render(t, Info{}, tuikit.VT)
	if !strings.Contains(got, "EXPANSE") {
		t.Fatal(got)
	}
}

// TestDumpScreens writes the console at 80x25 and 160x50 to $EXPANSE_TUI_DUMP for eyeballing.
func TestDumpScreens(t *testing.T) {
	dir := os.Getenv("EXPANSE_TUI_DUMP")
	if dir == "" {
		t.Skip("EXPANSE_TUI_DUMP not set")
	}
	degraded := healthyInfo()
	degraded.Arrays = []MDArray{{Name: "md126", Level: "raid1", Members: "[U_]", Total: 2, Active: 1, Sync: "recovery 12.5%"}}
	degraded.Health = Health{Overall: "degraded", Failing: []string{"system-mirror"}}
	fresh := healthyInfo()
	fresh.Cluster = nil
	down := fresh
	down.Agent = AgentState{Err: "agent socket /run/expanse/agent.sock not available"}
	down.Health = Health{}
	for name, in := range map[string]Info{"clustered": healthyInfo(), "degraded": degraded, "fresh": fresh, "agent-down": down} {
		for _, sz := range []tuikit.Size{tuikit.VT, {Cols: 160, Rows: 50}} {
			text := strings.Join(Render(in, sz, tuikit.Unicode), "\n")
			f := filepath.Join(dir, fmt.Sprintf("console-%s-%dx%d.txt", name, sz.Cols, sz.Rows))
			if err := os.WriteFile(f, []byte(text+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestLocalIPsSkipsLoopback(t *testing.T) {
	for _, ip := range LocalIPs() {
		if strings.HasPrefix(ip, "127.") || ip == "::1" {
			t.Fatalf("loopback listed: %v", LocalIPs())
		}
	}
}

func TestRenderShowsNodeIDOnlyWhenItAddsSomething(t *testing.T) {
	cases := []struct {
		name, id string
		want     bool
	}{
		{"pre-cluster uuid", "0f3c1a2b-9c1d-4e2f-8a3b-5c6d7e8f9a0b", false},
		{"same as hostname", "node-a", false},
		{"renamed host", "db-1", true},
	}
	for _, c := range cases {
		in := healthyInfo()
		in.NodeID = c.id
		got := render(t, in, tuikit.VT)
		if strings.Contains(got, "Node ID") != c.want || (c.want && !strings.Contains(got, c.id)) {
			t.Errorf("%s: Node ID row shown = %v, want %v:\n%s", c.name, !c.want, c.want, got)
		}
	}
}

func TestRenderHidesStaleFailingChecksWhenHealthyAndCountsOneNode(t *testing.T) {
	in := healthyInfo()
	in.Health.Failing = []string{"clock-sync"} // cached from boot, before chrony synced
	in.Cluster = &Cluster{Name: "lab", Role: "leader", QuorumHave: 1, QuorumNeed: 1, Nodes: 1}
	got := render(t, in, tuikit.VT)
	if strings.Contains(got, "clock-sync") {
		t.Errorf("a healthy node lists a failing check:\n%s", got)
	}
	if !strings.Contains(got, "1/1  (1 node)") {
		t.Errorf("want singular \"(1 node)\":\n%s", got)
	}
}
