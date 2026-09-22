package main

// Installer TUI: a minimal, dependency-free full-screen terminal UI.
// Keyboard-only, works at 80x24. Screens: welcome, disks, network,
// ssh keys, review, progress, done. (Cluster join is Phase 03.)

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/expanse/expanse/internal/install"
)

type tuiState struct {
	screen   int
	sel      int // list cursor
	disks    []install.Disk
	picked   map[int]bool
	netMode  string // dhcp | static
	hostname string
	sshKey   string
	typed    string // review "INSTALL" buffer
	errmsg   string
}

const (
	screenWelcome = iota
	screenDisks
	screenNetwork
	screenSSH
	screenReview
	screenProgress
	screenDone
)

// raw terminal handling ------------------------------------------------

type termios struct {
	Iflag, Oflag, Cflag, Lflag uint32
	Line                       uint8
	Cc                         [32]uint8
	Ispeed, Ospeed             uint32
}

func ioctl(fd, req uintptr, t *termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

func rawMode(on bool) error {
	var t termios
	if err := ioctl(os.Stdin.Fd(), syscall.TCGETS, &t); err != nil {
		return err
	}
	if on {
		t.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG
		t.Cc[syscall.VMIN] = 1
		t.Cc[syscall.VTIME] = 0
	} else {
		t.Lflag |= syscall.ECHO | syscall.ICANON | syscall.ISIG
	}
	return ioctl(os.Stdin.Fd(), syscall.TCSETS, &t)
}

func readKey() string {
	buf := make([]byte, 8)
	n, err := os.Stdin.Read(buf)
	if err != nil || n == 0 {
		return "q"
	}
	switch buf[0] {
	case 3, 4, 'q': // ^C, ^D, q
		return "q"
	case 13, 10:
		return "enter"
	case 9, 0x1b:
		if n > 1 && buf[1] == '[' {
			switch buf[2] {
			case 'A':
				return "up"
			case 'B':
				return "down"
			}
		}
		return "tab"
	case 127, 8:
		return "backspace"
	}
	return string(buf[:n])
}

// rendering ------------------------------------------------------------

func clear() { fmt.Print("\x1b[2J\x1b[H") }

func header(title string) {
	fmt.Printf("\x1b[7m %-78s \x1b[0m\r\n\r\n", strings.ToUpper(title))
}

func footer(help string) {
	fmt.Printf("\r\n\x1b[7m %-78s \x1b[0m\r\n", help)
}

func line(format string, a ...any) {
	fmt.Printf("\r"+format+"\r\n", a...)
}

// runTUI drives the whole interactive installer.
func runTUI() error {
	if err := rawMode(true); err != nil {
		return fmt.Errorf("put terminal in raw mode: %w", err)
	}
	defer func() { _ = rawMode(false) }()

	st := &tuiState{netMode: "dhcp", picked: map[int]bool{}}
	st.disks, _ = install.DetectDisks()

	for {
		clear()
		switch st.screen {
		case screenWelcome:
			drawWelcome(st)
		case screenDisks:
			drawDisks(st)
		case screenNetwork:
			drawNetwork(st)
		case screenSSH:
			drawSSH(st)
		case screenReview:
			drawReview(st)
		case screenProgress:
			runInstallProgress(st)
		case screenDone:
			drawDone(st)
		}
		if st.screen == screenProgress || st.screen == screenDone {
			// terminal screens; read one key to exit/advance
			if k := readKey(); k == "q" {
				return nil
			}
			continue
		}
		if !handleKey(st, readKey()) {
			return nil
		}
	}
}

// handleKey returns false when the TUI should exit.
func handleKey(st *tuiState, key string) bool {
	switch st.screen {
	case screenWelcome:
		return key == "enter" || key == "q"
	case screenDisks:
		if key == " " && st.sel < len(st.disks) {
			st.picked[st.sel] = !st.picked[st.sel]
			return true
		}
	case screenNetwork:
		if st.sel == 2 {
			// hostname entry
			switch key {
			case "backspace":
				st.hostname = st.hostname[:max(0, len(st.hostname)-1)]
				return true
			case "enter":
				nextScreen(st)
				return true
			}
			if len(key) == 1 {
				st.hostname += key
				return true
			}
		}
		if key == " " || key == "enter" {
			switch st.sel {
			case 0:
				st.netMode = "dhcp"
				nextScreen(st)
				return true
			case 1:
				st.netMode = "static"
			}
		}
	case screenSSH:
		switch key {
		case "backspace":
			st.sshKey = st.sshKey[:max(0, len(st.sshKey)-1)]
			return true
		case "enter":
			nextScreen(st)
			return true
		}
		if len(key) == 1 {
			st.sshKey += key
			return true
		}
		return true
	case screenReview:
		if key == "backspace" {
			st.typed = st.typed[:max(0, len(st.typed)-1)]
		} else if len(key) == 1 {
			st.typed += key
		}
		if st.typed == "INSTALL" {
			st.screen = screenProgress
			return true
		}
		if strings.HasPrefix("INSTALL", st.typed) {
			return true
		}
		st.typed = ""
		return true
	}
	max := 0
	switch st.screen {
	case screenDisks:
		max = len(st.disks) + 1 // + layout row
	case screenNetwork:
		max = 3
	}
	switch key {
	case "up":
		st.sel = (st.sel + max + 1) % max
	case "down":
		st.sel = (st.sel + 1) % max
	case "enter":
		nextScreen(st)
	case "backspace":
		if st.screen > screenDisks {
			st.screen--
		}
	case "q":
		return false
	}
	return true
}

func nextScreen(st *tuiState) {
	switch st.screen {
	case screenDisks:
		if len(st.picked) == 0 {
			st.errmsg = "select at least one disk with <space>"
			return
		}
		st.errmsg = ""
		st.screen = screenNetwork
	case screenNetwork:
		st.screen = screenSSH
	case screenSSH:
		st.screen = screenReview
	}
}

func drawWelcome(st *tuiState) {
	header("expanse installer")
	line("Welcome. This will turn this machine into an Expanse node.")
	line("")
	line("Detected hardware:")
	line("  disks:")
	for _, d := range st.disks {
		line("    %-14s %6s  %s", d.Path, d.Size, d.Model)
	}
	line("")
	line("The install WILL DESTROY all data on the disks you select.")
	footer("ENTER continue                         q quit")
}

func drawDisks(st *tuiState) {
	header("disk selection")
	line("Select target disk(s) with <space>. Space toggles; ENTER continues.")
	line("")
	for i, d := range st.disks {
		mark := " "
		if st.picked[i] {
			mark = "x"
		}
		cur := " "
		if st.sel == i {
			cur = ">"
		}
		content := "empty"
		if d.NonEmpty() {
			content = "CONTAINS DATA"
		}
		line("%s[%s] %-12s %6s  %-16s %-14s", cur, mark, d.Path, d.Size, d.Model, content)
	}
	cur := " "
	if st.sel == len(st.disks) {
		cur = ">"
	}
	n := len(st.picked)
	layout := "single"
	if n >= 2 {
		layout = "mirror"
	}
	line("%s    layout for %d disk(s): %s", cur, n, layout)
	if st.errmsg != "" {
		line("")
		line("\x1b[31m%s\x1b[0m", st.errmsg)
	}
	footer("UP/DOWN move  SPACE toggle  ENTER continue  BACKSPACE back  q quit")
}

func drawNetwork(st *tuiState) {
	header("network")
	line("How should this node get an address?")
	line("")
	cur := " "
	if st.sel == 0 {
		cur = ">"
	}
	mark := " "
	if st.netMode == "dhcp" {
		mark = "x"
	}
	line("%s[%s] DHCP (default — recommended)", cur, mark)
	cur = " "
	if st.sel == 1 {
		cur = ">"
	}
	mark = " "
	if st.netMode == "static" {
		mark = "x"
	}
	line("%s[%s] Static address", cur, mark)
	cur = " "
	if st.sel == 2 {
		cur = ">"
	}
	line("%s    hostname: %s", cur, st.hostname)
	footer("UP/DOWN move  SPACE/ENTER select  BACKSPACE back  q quit")
}

func drawSSH(st *tuiState) {
	header("ssh access")
	line("Paste an SSH public key for operator access (one line).")
	line("You can also use gh:<username> to fetch keys from GitHub.")
	line("")
	if st.sel == 0 {
		line("> key: %s_", st.sshKey)
	} else {
		line("  key: %s", st.sshKey)
	}
	footer("type key  BACKSPACE edit  ENTER continue (empty = no key)  q quit")
}

func drawReview(st *tuiState) {
	header("review")
	line("Hostname:  %s (default: expanse-<node-id> if blank)", st.hostname)
	var picked []install.Disk
	for i, d := range st.disks {
		if st.picked[i] {
			picked = append(picked, d)
		}
	}
	line("Network:   %s", st.netMode)
	line("SSH key:   %s", orDash(st.sshKey))
	line("Timezone:  UTC")
	line("")
	line("\x1b[31mWILL DESTROY:\x1b[0m")
	for _, d := range picked {
		line("  %s (%s %s)", d.Path, d.Model, d.Size)
	}
	line("")
	line("Root filesystem is wiped on EVERY REBOOT. All state lives in /persist.")
	line("")
	line("Type INSTALL (all caps) to proceed: %s", st.typed)
	footer("type INSTALL to begin  BACKSPACE back  q quit")
}

func runInstallProgress(st *tuiState) {
	header("installing")
	line("Running install stages. Output is also printed to the console.")
	line("")
	// Build the config from the TUI choices.
	cfg := install.DefaultConfig()
	for i, d := range st.disks {
		if st.picked[i] {
			cfg.Disks.Devices = append(cfg.Disks.Devices, d.Path)
		}
	}
	cfg.Network.Mode = st.netMode
	cfg.Hostname = st.hostname
	if st.sshKey != "" {
		cfg.SSH.AuthorizedKeys = []string{st.sshKey}
	}
	cfg.Disks.Force = true // the typed INSTALL confirmation is the gate

	tmp, err := os.CreateTemp("", "expanse-install-*.yaml")
	if err != nil {
		st.errmsg = err.Error()
		st.screen = screenDone
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := cfg.WriteYAML(tmp.Name()); err != nil {
		st.errmsg = err.Error()
		st.screen = screenDone
		return
	}

	err = install.Run(install.Options{
		ConfigPath: tmp.Name(),
		Force:      true,
		Logger: func(f string, a ...any) {
			line(f, a...)
		},
	})
	if err != nil {
		line("")
		line("\x1b[31mFAILED: %v\x1b[0m", err)
		st.errmsg = err.Error()
	}
	st.screen = screenDone
}

func drawDone(st *tuiState) {
	header("done")
	if st.errmsg != "" {
		line("Install failed: %s", st.errmsg)
		line("Fix the problem and re-run; the installer is safe to retry.")
	} else {
		id, err := install.LoadIdentity("/mnt/persist/expanse/identity")
		if err == nil {
			line("Node ID: %s", id.NodeID)
		}
		line("Reboot into the installed system:")
		line("  # reboot")
		line("Then visit http://<node-ip>:8443")
	}
	footer("q quit")
}

func orDash(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
