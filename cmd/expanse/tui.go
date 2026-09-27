package main

// Installer TUI: a minimal, dependency-free full-screen terminal UI.
// Keyboard-only, works at 80x24. tuiModel is the pure screen logic; tui_term.go drives a terminal.

import (
	"fmt"
	"strings"

	"github.com/expanse/expanse/internal/install"
)

const (
	screenWelcome = iota
	screenDisks
	screenNetwork
	screenSSH
	screenReview
	screenProgress
	screenDone
)

// installLogPath receives the whole install's output, so a failure can be read after the screen clears.
const installLogPath = "/tmp/expanse-install.log"

type tuiModel struct {
	screen   int
	sel      int // disk list cursor
	disks    []install.Disk
	picked   map[int]bool
	hostname string
	sshKey   string
	typed    string // review "INSTALL" buffer
	errmsg   string
	nodeID   string
}

func newTUIModel(disks []install.Disk) *tuiModel {
	return &tuiModel{disks: disks, picked: map[int]bool{}}
}

// Update applies one key (see parseKeys) and reports whether the TUI should exit.
func (m *tuiModel) Update(key string) bool {
	if key == "ctrl-c" {
		return true
	}
	switch m.screen {
	case screenWelcome:
		if key == "enter" {
			m.screen = screenDisks
		}
		return key == "q"
	case screenDisks:
		return m.updateDisks(key)
	case screenNetwork:
		m.edit(&m.hostname, key, screenSSH)
	case screenSSH:
		m.edit(&m.sshKey, key, screenReview)
	case screenReview:
		m.updateReview(key)
	case screenDone:
		return true
	}
	return false
}

func (m *tuiModel) updateDisks(key string) bool {
	switch key {
	case "up":
		m.sel = (m.sel + len(m.disks) - 1) % max(1, len(m.disks))
	case "down":
		m.sel = (m.sel + 1) % max(1, len(m.disks))
	case " ":
		if m.sel < len(m.disks) {
			m.picked[m.sel] = !m.picked[m.sel]
		}
	case "enter":
		if len(m.pickedPaths()) == 0 {
			m.errmsg = "select at least one disk with <space>"
			return false
		}
		m.errmsg = ""
		m.screen = screenNetwork
	case "backspace":
		m.screen = screenWelcome
	case "q":
		return true
	}
	return false
}

// edit handles a text field: typed and pasted text append, backspace deletes (or goes back when empty).
func (m *tuiModel) edit(field *string, key string, next int) {
	switch key {
	case "enter":
		m.screen = next
	case "backspace":
		if *field == "" {
			m.screen--
			return
		}
		*field = (*field)[:len(*field)-1]
	case "up", "down", "tab":
	default:
		*field += key
	}
}

func (m *tuiModel) updateReview(key string) {
	switch key {
	case "backspace":
		if m.typed == "" {
			m.screen--
			return
		}
		m.typed = m.typed[:len(m.typed)-1]
	case "enter", "up", "down", "tab":
	default:
		m.typed += key
		if !strings.HasPrefix("INSTALL", m.typed) {
			m.typed = ""
		}
		if m.typed == "INSTALL" {
			m.screen = screenProgress
		}
	}
}

// finish records the install's outcome and shows the done screen.
func (m *tuiModel) finish(nodeID string, err error) {
	m.nodeID = nodeID
	m.errmsg = ""
	if err != nil {
		m.errmsg = err.Error()
	}
	m.screen = screenDone
}

func (m *tuiModel) pickedPaths() []string {
	var paths []string
	for i, d := range m.disks {
		if m.picked[i] {
			paths = append(paths, d.Path)
		}
	}
	return paths
}

func (m *tuiModel) installConfig() *install.Config {
	cfg := install.DefaultConfig()
	cfg.Disks.Devices = m.pickedPaths()
	cfg.Disks.Force = true // the typed INSTALL confirmation is the gate
	cfg.Network.Mode = "dhcp"
	cfg.Hostname = m.hostname
	if m.sshKey != "" {
		cfg.SSH.AuthorizedKeys = []string{m.sshKey}
	}
	return cfg
}

// View renders the current screen; lines end in \r\n for a raw-mode terminal.
func (m *tuiModel) View() string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\r\n", a...) }
	bar := func(s string) { line("\x1b[7m %-78s \x1b[0m", s) }
	switch m.screen {
	case screenWelcome:
		bar("EXPANSE INSTALLER")
		line("")
		line("This will turn this machine into an Expanse node.")
		line("")
		line("Detected disks:")
		for _, d := range m.disks {
			line("  %-14s %6s  %s", d.Path, d.Size, d.Model)
		}
		if len(m.disks) == 0 {
			line("  (none found -- check lsblk)")
		}
		line("")
		line("The install WILL DESTROY all data on the disks you select.")
		line("")
		bar("ENTER continue   q quit")
	case screenDisks:
		bar("DISK SELECTION")
		line("")
		line("Pick target disk(s) with SPACE. 1 disk = single layout, 2+ = mirror.")
		line("")
		for i, d := range m.disks {
			cur, mark, content := " ", " ", "empty"
			if m.sel == i {
				cur = ">"
			}
			if m.picked[i] {
				mark = "x"
			}
			if d.NonEmpty() {
				content = "CONTAINS DATA"
			}
			line("%s[%s] %-12s %6s  %-16s %s", cur, mark, d.Path, d.Size, d.Model, content)
		}
		if m.errmsg != "" {
			line("")
			line("\x1b[31m%s\x1b[0m", m.errmsg)
		}
		line("")
		bar("UP/DOWN move  SPACE toggle  ENTER continue  BACKSPACE back  q quit")
	case screenNetwork:
		bar("NETWORK")
		line("")
		line("The node gets its address by DHCP. For a static address, quit and run")
		line("`expanse install --config <file>` (see docs/INSTALL.md).")
		line("")
		line("Hostname (blank = expanse-<node-id prefix>): %s_", m.hostname)
		line("")
		bar("type hostname  ENTER continue  BACKSPACE edit/back  Ctrl-C quit")
	case screenSSH:
		bar("SSH ACCESS")
		line("")
		line("Paste an SSH public key (one line), or gh:<username> to fetch from GitHub.")
		line("The installed node has no root password: this key is how you log in.")
		line("")
		line("key: %s_", m.sshKey)
		line("")
		bar("type/paste key  ENTER continue  BACKSPACE edit/back  Ctrl-C quit")
	case screenReview:
		bar("REVIEW")
		line("")
		line("Hostname:  %s", orDefault(m.hostname, "expanse-<node-id prefix>"))
		line("Network:   dhcp")
		line("SSH key:   %s", orDefault(m.sshKey, "(none)"))
		if m.sshKey == "" {
			line("\x1b[31m           no SSH key: you will not be able to log in to this node\x1b[0m")
		}
		line("")
		line("\x1b[31mWILL DESTROY:\x1b[0m")
		for i, d := range m.disks {
			if m.picked[i] {
				line("  %s (%s %s)", d.Path, d.Model, d.Size)
			}
		}
		line("")
		line("The root filesystem is wiped on EVERY REBOOT. All state lives in /persist.")
		line("")
		line("Type INSTALL (all caps) to proceed: %s_", m.typed)
		line("")
		bar("type INSTALL to begin  BACKSPACE edit/back  Ctrl-C quit")
	case screenProgress:
		bar("INSTALLING")
		line("")
		line("Install output follows (also in %s).", installLogPath)
		line("")
	case screenDone:
		m.viewDone(line, bar)
	}
	return b.String()
}

func (m *tuiModel) viewDone(line func(string, ...any), bar func(string)) {
	line("")
	if m.errmsg != "" {
		bar("INSTALL FAILED")
		line("")
		line("\x1b[31m%s\x1b[0m", m.errmsg)
		line("")
		line("Full output: %s. Fix the problem and run `expanse install --tui` again;", installLogPath)
		line("the installer is safe to retry.")
	} else {
		bar("INSTALL COMPLETE")
		line("")
		line("Node ID: %s", orDefault(m.nodeID, "(unreadable)"))
		line("")
		line("Remove the installer media, reboot, then log in with your key:")
		line("  ssh root@<node-ip>")
	}
	line("")
	bar("any key: exit to a shell")
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
