package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/install"
)

func testDisks() []install.Disk {
	return []install.Disk{
		{Path: "/dev/vda", Size: "32G", Model: "QEMU HARDDISK"},
		{Path: "/dev/vdb", Size: "8G", Model: "QEMU HARDDISK"},
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

func TestParseKeys(t *testing.T) {
	cases := map[string][]string{
		"\r":                          {"enter"},
		"\r\n":                        {"enter"},
		"\x1b[A\x1b[B":                {"up", "down"},
		"\x7f":                        {"backspace"},
		"\x03":                        {"ctrl-c"},
		" ":                           {" "},
		"q":                           {"q"},
		"ssh-ed25519 AAAAq me@host\n": {"ssh-ed25519 AAAAq me@host", "enter"},
		"\x1b[C":                      nil, // unhandled escape sequences are dropped whole
	}
	for in, want := range cases {
		if got := parseKeys([]byte(in)); !reflect.DeepEqual(got, want) {
			t.Errorf("parseKeys(%q) = %q, want %q", in, got, want)
		}
	}
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

func TestReviewWarnsWithoutSSHKey(t *testing.T) {
	m := newTUIModel(testDisks())
	feed(m, "enter", " ", "enter", "enter", "enter")
	if !strings.Contains(m.View(), "no SSH key") {
		t.Fatalf("review without a key does not warn:\n%s", m.View())
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

func TestDoneScreenShowsResult(t *testing.T) {
	m := newTUIModel(testDisks())
	m.finish("0f3c-node", nil)
	if m.screen != screenDone || !strings.Contains(m.View(), "0f3c-node") {
		t.Fatalf("done view:\n%s", m.View())
	}
	if !feed(m, "enter") {
		t.Fatal("a key on the done screen did not exit")
	}
}
