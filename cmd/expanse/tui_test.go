package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/install"
	"github.com/expanse/expanse/internal/tuikit"
)

func testDisks() []install.Disk {
	return []install.Disk{
		{Name: "vda", Path: "/dev/vda", Size: "32G", Model: "QEMU HARDDISK", Type: "disk"},
		{Name: "vdb", Path: "/dev/vdb", Size: "8G", Model: "QEMU HARDDISK", Type: "disk", FSType: "ext4"},
		{
			Name: "nvme0n1", Path: "/dev/nvme0n1", Size: "1.8T", Model: "Samsung SSD 980 PRO 2TB", Type: "disk",
			Children: []install.DiskPart{{Name: "nvme0n1p1", FSType: "vfat"}, {Name: "nvme0n1p2", FSType: "btrfs"}},
		},
	}
}

func feed(m *tuiModel, keys ...string) (quit bool) {
	for _, k := range keys {
		if m.Update(k) {
			return true
		}
	}
	return false
}

// screen renders the model at sz and returns the plain text, checking the frame's shape.
func screen(t *testing.T, m *tuiModel, sz tuikit.Size) string {
	t.Helper()
	lines := m.View(sz, tuikit.Unicode)
	if len(lines) != sz.Rows {
		t.Fatalf("screen %d at %v: %d lines, want %d", m.screen, sz, len(lines), sz.Rows)
	}
	for i, l := range lines {
		if w := tuikit.Width(l); w > sz.Cols {
			t.Fatalf("screen %d at %v: line %d is %d wide: %q", m.screen, sz, i, w, l)
		}
	}
	return tuikit.Strip(strings.Join(lines, "\n"))
}

func TestWelcomeEnterGoesToDisks(t *testing.T) {
	m := newTUIModel(testDisks())
	if feed(m, "enter") || m.screen != screenDisks {
		t.Fatalf("after enter on welcome: screen %d, want disks", m.screen)
	}
}

func TestWelcomeQuitsOnlyOnQOrCtrlC(t *testing.T) {
	m := newTUIModel(testDisks())
	if feed(m, "x") || m.screen != screenWelcome {
		t.Fatalf("a stray key left welcome or quit (screen %d)", m.screen)
	}
	if !feed(m, "q") {
		t.Fatal("q on welcome did not quit")
	}
	if !feed(newTUIModel(testDisks()), "ctrl-c") {
		t.Fatal("ctrl-c did not quit")
	}
}

func TestDisksNeedOnePicked(t *testing.T) {
	m := newTUIModel(testDisks())
	feed(m, "enter", "enter")
	if m.screen != screenDisks || m.errmsg == "" {
		t.Fatalf("continued with no disk picked (screen %d, err %q)", m.screen, m.errmsg)
	}
	feed(m, "down", " ", "enter")
	if m.screen != screenNetwork {
		t.Fatalf("screen %d after picking a disk, want network", m.screen)
	}
	if got := m.pickedPaths(); !reflect.DeepEqual(got, []string{"/dev/vdb"}) {
		t.Fatalf("picked %v, want [/dev/vdb]", got)
	}
}

func TestPastedKeyAndQAreTextOnEntryScreens(t *testing.T) {
	m := newTUIModel(testDisks())
	feed(m, "enter", " ", "enter", "node", "q", "enter", "ssh-ed25519 AAAA", "q", " me@host", "enter")
	if m.screen != screenReview {
		t.Fatalf("screen %d, want review", m.screen)
	}
	if m.hostname != "nodeq" || m.sshKey != "ssh-ed25519 AAAAq me@host" {
		t.Fatalf("hostname %q ssh key %q", m.hostname, m.sshKey)
	}
}

func TestBackspaceEditsThenGoesBack(t *testing.T) {
	m := newTUIModel(testDisks())
	feed(m, "enter", " ", "enter", "ab", "backspace")
	if m.hostname != "a" || m.screen != screenNetwork {
		t.Fatalf("hostname %q screen %d", m.hostname, m.screen)
	}
	feed(m, "backspace", "backspace")
	if m.screen != screenDisks {
		t.Fatalf("backspace on an empty field: screen %d, want disks", m.screen)
	}
}

func TestReviewStartsInstallOnlyOnINSTALL(t *testing.T) {
	m := newTUIModel(testDisks())
	feed(m, "enter", " ", "enter", "enter", "enter", "INSTAL")
	if m.screen != screenReview {
		t.Fatalf("screen %d before the last letter, want review", m.screen)
	}
	feed(m, "L")
	if m.screen != screenProgress {
		t.Fatalf("screen %d after INSTALL, want progress", m.screen)
	}
}

func TestReviewWarnsWithoutSSHKeyAndListsTheDestruction(t *testing.T) {
	m := newTUIModel(testDisks())
	feed(m, "enter", " ", "down", " ", "enter", "enter", "enter")
	got := screen(t, m, tuikit.VT)
	for _, want := range []string{"no SSH key", "WILL DESTROY", "/dev/vda", "/dev/vdb", "mirror", "Step 5 of 6"} {
		if !strings.Contains(got, want) {
			t.Fatalf("review lacks %q:\n%s", want, got)
		}
	}
}

func TestInstallConfigFromChoices(t *testing.T) {
	m := newTUIModel(testDisks())
	feed(m, "enter", " ", "enter", "n1", "enter", "ssh-ed25519 AAAA me", "enter")
	cfg := m.installConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config from the TUI does not validate: %v", err)
	}
	if !reflect.DeepEqual(cfg.Disks.Devices, []string{"/dev/vda"}) || cfg.Hostname != "n1" ||
		cfg.Network.Mode != "dhcp" || !reflect.DeepEqual(cfg.SSH.AuthorizedKeys, []string{"ssh-ed25519 AAAA me"}) {
		t.Fatalf("config %+v", cfg)
	}
}

func TestEveryScreenFitsTheVTAndLargerTerminals(t *testing.T) {
	m := newTUIModel(testDisks())
	steps := []struct {
		keys []string
		want string
	}{
		{nil, "Step 1 of 6"},
		{[]string{"enter"}, "Step 2 of 6"},
		{[]string{" ", "enter"}, "Step 3 of 6"},
		{[]string{"a-very-long-hostname-that-goes-on-and-on-and-on-and-on-and-on-and-on-and-on-and-on", "enter"}, "Step 4 of 6"},
		{[]string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItuitestAAAAC3NzaC1lZDI1NTE5AAAAItuitestAAAAC3NzaC1lZDI1NTE5 tui@test", "enter"}, "Step 5 of 6"},
		{[]string{"INSTALL"}, "Step 6 of 6"},
	}
	for _, st := range steps {
		feed(m, st.keys...)
		for _, sz := range []tuikit.Size{tuikit.VT, {Cols: 160, Rows: 50}, {Cols: 100, Rows: 30}} {
			if got := screen(t, m, sz); !strings.Contains(got, st.want) {
				t.Fatalf("screen %d at %v lacks %q:\n%s", m.screen, sz, st.want, got)
			}
		}
	}
}

func TestDiskTableShowsModelSizeTypeAndContents(t *testing.T) {
	m := newTUIModel(testDisks())
	feed(m, "enter", " ")
	got := screen(t, m, tuikit.VT)
	for _, want := range []string{"DEVICE", "SIZE", "TYPE", "MODEL", "CONTENTS", "empty", "ext4", "vfat, btrfs", "nvme", "[x] /dev/vda", "[ ] /dev/vdb"} {
		if !strings.Contains(got, want) {
			t.Fatalf("disk table lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "1 disk selected") {
		t.Fatalf("no selection summary:\n%s", got)
	}
}

func TestProgressShowsPhaseElapsedAndLogTail(t *testing.T) {
	m := newTUIModel(testDisks())
	feed(m, "enter", " ", "enter", "enter", "enter", "INSTALL")
	m.started = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.appendLog("==> preflight: check hardware requirements")
	m.appendLog("<== preflight done (0.1s)")
	m.appendLog("==> partition: partition + format via disko")
	for i := 0; i < 200; i++ {
		m.appendLog("mkfs output line " + strings.Repeat("x", i%90))
	}
	m.appendLog("+ declare -a extraArgs")
	m.appendLog("+ mountpoint=/mnt")
	m.appendLog("++ mktemp -d")
	m.appendLog("+++ dirname /tmp/x")
	m.appendLog("the last line")
	m.tick(m.started.Add(83 * time.Second))
	got := screen(t, m, tuikit.VT)
	for _, want := range []string{"partition", "4 of 9", "01:23", "the last line", "[#"} {
		if !strings.Contains(got, want) {
			t.Fatalf("progress lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "preflight:") {
		t.Fatalf("progress shows the whole log instead of a tail:\n%s", got)
	}
	if strings.Contains(got, "+ declare") || strings.Contains(got, "+ mountpoint") || strings.Contains(got, "mktemp") || strings.Contains(got, "dirname") {
		t.Fatalf("the shell trace is on screen:\n%s", got)
	}
}

func TestDoneScreenShowsNextSteps(t *testing.T) {
	m := newTUIModel(testDisks())
	m.finish(installResult{nodeID: "0f3c1a2b-0000-4000-8000-000000000000", ips: []string{"192.168.1.10", "10.0.2.15"}})
	got := screen(t, m, tuikit.VT)
	for _, want := range []string{
		"INSTALL COMPLETE", "0f3c1a2b-0000", "https://192.168.1.10:8443", "10.0.2.15",
		"expanse cluster init --expect 1", "journalctl -u expansed", "any key",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("done screen lacks %q:\n%s", want, got)
		}
	}
	if !feed(m, "enter") {
		t.Fatal("a key on the done screen did not exit")
	}
}

func TestDoneScreenShowsFailure(t *testing.T) {
	m := newTUIModel(testDisks())
	m.finish(installResult{err: errors.New("stage partition: disko exited 1")})
	got := screen(t, m, tuikit.VT)
	for _, want := range []string{"INSTALL FAILED", "disko exited 1", installLogPath, "safe to retry"} {
		if !strings.Contains(got, want) {
			t.Fatalf("failure screen lacks %q:\n%s", want, got)
		}
	}
}

func TestManyIPsAndLongHostnameStillFit(t *testing.T) {
	m := newTUIModel(testDisks())
	m.hostname = strings.Repeat("h", 120)
	var ips []string
	for i := 0; i < 40; i++ {
		ips = append(ips, "10.0.0."+string(rune('0'+i%10)))
	}
	m.finish(installResult{nodeID: "id", ips: ips})
	if got := screen(t, m, tuikit.VT); !strings.Contains(got, "more") {
		t.Fatalf("40 addresses should be summarised:\n%s", got)
	}
}

// TestDumpScreens writes every screen at 80x25 and 160x50 to $EXPANSE_TUI_DUMP for eyeballing.
func TestDumpScreens(t *testing.T) {
	dir := os.Getenv("EXPANSE_TUI_DUMP")
	if dir == "" {
		t.Skip("EXPANSE_TUI_DUMP not set")
	}
	m := newTUIModel(testDisks())
	dump := func(name string) {
		for _, sz := range []tuikit.Size{tuikit.VT, {Cols: 160, Rows: 50}} {
			text := strings.Join(m.View(sz, tuikit.Unicode), "\n")
			f := filepath.Join(dir, fmt.Sprintf("installer-%s-%dx%d.txt", name, sz.Cols, sz.Rows))
			if err := os.WriteFile(f, []byte(text+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	dump("1-welcome")
	feed(m, "enter", "down", " ", "down", " ")
	dump("2-disks")
	feed(m, "enter", "node-a")
	dump("3-network")
	feed(m, "enter", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItuitest tui@test")
	dump("4-ssh")
	feed(m, "enter")
	dump("5-review")
	feed(m, "INSTALL")
	m.started = time.Now().Add(-95 * time.Second)
	for _, l := range []string{
		"==> preflight: check hardware requirements (arch, RAM >= 2G, disk >= 20G, network)",
		"<== preflight done (0.0s)", "==> detect: detect target disks", "<== detect done (0.0s)",
		"==> confirm: confirm the destructive plan (WILL WIPE selected disks)", "<== confirm done (0.0s)",
		"==> partition: partition + format via disko", "+ sgdisk --zap-all /dev/vdb", "+ mkfs.btrfs -L system /dev/vdb2",
		"btrfs-progs v6.9", "See https://btrfs.readthedocs.io for more information.", "+ mount /dev/vdb2 /mnt",
	} {
		m.appendLog(l)
	}
	m.tick(time.Now())
	dump("6-progress")
	m.finish(installResult{nodeID: "0f3c1a2b-9c1d-4e2f-8a3b-5c6d7e8f9a0b", ips: []string{"192.168.1.10", "fd00::a"}})
	dump("7-done")
	m.finish(installResult{err: errors.New("stage partition: disko: exit status 1")})
	dump("8-failed")
}
