package main

// Installer TUI: a dependency-free full-screen terminal UI on the tuikit frame.
// tuiModel is the pure screen logic (Update/View); tui_term.go drives a terminal.

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/install"
	"github.com/expanse/expanse/internal/tuikit"
	"github.com/expanse/expanse/internal/version"
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

const tuiSteps = 6

// installLogPath receives the whole install's output, so a failure can be read after the screen clears.
const installLogPath = "/tmp/expanse-install.log"

// maxLogLines is how much install output the progress screen keeps; the file has it all.
const maxLogLines = 200

type tuiModel struct {
	screen   int
	sel      int // disk list cursor
	disks    []install.Disk
	picked   map[int]bool
	hostname string
	sshKey   string
	typed    string // review "INSTALL" buffer
	errmsg   string

	// Progress and done state.
	started time.Time
	now     time.Time
	log     []string
	stage   string
	nodeID  string
	ips     []string
}

// installResult is what the install goroutine hands back to the model.
type installResult struct {
	nodeID string
	ips    []string
	err    error
}

func newTUIModel(disks []install.Disk) *tuiModel {
	return &tuiModel{disks: disks, picked: map[int]bool{}}
}

// Update applies one key (see tuikit.ParseKeys) and reports whether the TUI should exit.
func (m *tuiModel) Update(key string) bool {
	if m.screen == screenProgress {
		return false // nothing interrupts a running install
	}
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
			m.started = time.Now()
			m.now = m.started
		}
	}
}

// appendLog records one line of install output for the on-screen tail and tracks the stage
// from "==> name: ..." lines. disko's `set -x` trace ("+ cmd", "++ cmd") stays in the log file only.
func (m *tuiModel) appendLog(line string) {
	if name, _, ok := strings.Cut(strings.TrimPrefix(line, "==> "), ":"); ok && strings.HasPrefix(line, "==> ") {
		m.stage = name
	}
	if isXtrace(line) {
		return
	}
	m.log = append(m.log, line)
	if len(m.log) > maxLogLines {
		m.log = m.log[len(m.log)-maxLogLines:]
	}
}

// isXtrace matches a shell trace line: one or more '+' (one per subshell level), then a space.
func isXtrace(line string) bool {
	rest := strings.TrimLeft(line, "+")
	return len(rest) < len(line) && strings.HasPrefix(rest, " ")
}

// tick advances the clock the progress screen shows.
func (m *tuiModel) tick(now time.Time) { m.now = now }

// finish records the install's outcome and shows the done screen.
func (m *tuiModel) finish(r installResult) {
	m.nodeID, m.ips, m.errmsg = r.nodeID, r.ips, ""
	if r.err != nil {
		m.errmsg = r.err.Error()
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

// layoutName describes the disk layout the selection implies.
func (m *tuiModel) layoutName() string {
	switch n := len(m.pickedPaths()); n {
	case 0:
		return "no disk selected"
	case 1:
		return "1 disk selected: single layout"
	default:
		return fmt.Sprintf("%d disks selected: mirror layout (md RAID1 system, either one boots)", n)
	}
}

// layoutShort is the review's one-word layout name.
func (m *tuiModel) layoutShort() string {
	if n := len(m.pickedPaths()); n > 1 {
		return fmt.Sprintf("mirror (%d disks, md RAID1 system)", n)
	}
	return "single (1 disk)"
}

// View renders the current screen as exactly sz.Rows lines for the terminal.
func (m *tuiModel) View(sz tuikit.Size, g tuikit.Glyphs) []string {
	f := tuikit.Frame{Header: "EXPANSE INSTALLER " + version.Get().Version}
	step := min(m.screen+1, tuiSteps)
	f.HeaderRight = fmt.Sprintf("Step %d of %d", step, tuiSteps)
	inner := f.Inner(sz)
	switch m.screen {
	case screenWelcome:
		f.Title, f.Body = "Welcome", m.viewWelcome(inner)
		f.Footer = tuikit.Keys("ENTER", "continue", "q", "quit")
	case screenDisks:
		f.Title, f.Body = "Disks", m.viewDisks(inner)
		f.Footer = tuikit.Keys("UP/DOWN", "move", "SPACE", "toggle", "ENTER", "continue", "BACKSPACE", "back", "q", "quit")
	case screenNetwork:
		f.Title, f.Body = "Network", m.viewNetwork(inner)
		f.Footer = tuikit.Keys("type", "hostname", "ENTER", "continue", "BACKSPACE", "edit/back", "Ctrl-C", "quit")
	case screenSSH:
		f.Title, f.Body = "SSH access", m.viewSSH(inner)
		f.Footer = tuikit.Keys("type/paste", "key", "ENTER", "continue", "BACKSPACE", "edit/back", "Ctrl-C", "quit")
	case screenReview:
		f.Title, f.Body = "Review", m.viewReview(inner)
		f.Footer = tuikit.Keys("type INSTALL", "begin", "BACKSPACE", "edit/back", "Ctrl-C", "quit")
	case screenProgress:
		f.Title, f.Body = "Installing", m.viewProgress(inner, g)
		f.Footer = tuikit.Dim + "Installing; keys are ignored until it finishes." + tuikit.Reset
	case screenDone:
		f.Title, f.Body = "Done", m.viewDone(inner)
		if m.errmsg != "" {
			f.Title = "Failed"
		}
		f.Footer = tuikit.Keys("any key", "exit to a shell")
	}
	return f.Render(sz, g)
}

func (m *tuiModel) viewWelcome(inner tuikit.Size) []string {
	body := []string{
		"",
		"This installer turns this machine into an Expanse node: a NixOS system with",
		"the Expanse agent, a btrfs root wiped on every boot, and /persist for state.",
		"",
		tuikit.Bold + "Detected disks" + tuikit.Reset,
	}
	body = append(body, m.diskTable(inner.Cols, false)...)
	if len(m.disks) == 0 {
		body = append(body, tuikit.Yellow+"  (none found -- check lsblk)"+tuikit.Reset)
	}
	body = append(body, "",
		"Steps: disks, network, SSH key, review, install.",
		tuikit.Red+tuikit.Bold+"The install WILL DESTROY all data on the disks you select."+tuikit.Reset)
	if m.errmsg != "" {
		body = append(body, "", tuikit.Red+m.errmsg+tuikit.Reset)
	}
	return body
}

// diskTable lists the disks; with cursor, rows carry the [x] marks and the cursor row is highlighted.
func (m *tuiModel) diskTable(width int, cursor bool) []string {
	headers := []string{"DEVICE", "SIZE", "TYPE", "MODEL", "CONTENTS"}
	if cursor {
		headers[0] = "    DEVICE"
	}
	var rows [][]string
	for i, d := range m.disks {
		dev := d.Path
		if cursor {
			mark := "[ ]"
			if m.picked[i] {
				mark = tuikit.Green + tuikit.Bold + "[x]" + tuikit.Reset
			}
			dev = mark + " " + d.Path
		}
		rows = append(rows, []string{dev, d.Size, diskKind(d), d.Model, diskContents(d)})
	}
	lines := tuikit.Table(headers, rows, width)
	if cursor && m.sel < len(m.disks) {
		i := m.sel + 1
		lines[i] = tuikit.Reverse + tuikit.Pad(tuikit.Strip(lines[i]), width) + tuikit.Reset
	}
	return lines
}

// diskKind is a short bus/type word for the table's TYPE column (lsblk -p names are paths).
func diskKind(d install.Disk) string {
	switch base := filepath.Base(d.Path); {
	case d.RM:
		return "usb"
	case strings.HasPrefix(base, "nvme"):
		return "nvme"
	case strings.HasPrefix(base, "vd"):
		return "virtio"
	case d.Type != "":
		return d.Type
	}
	return "disk"
}

// diskContents names what a disk holds: "empty", a filesystem, or its partitions' filesystems.
func diskContents(d install.Disk) string {
	if !d.NonEmpty() {
		return "empty"
	}
	var fs []string
	if d.FSType != "" {
		fs = append(fs, d.FSType)
	}
	for _, c := range d.Children {
		if c.FSType != "" {
			fs = append(fs, c.FSType)
		}
	}
	desc := "partitioned"
	if len(fs) > 0 {
		desc = strings.Join(fs, ", ")
	}
	return tuikit.Yellow + "DATA: " + desc + tuikit.Reset
}

func (m *tuiModel) viewDisks(inner tuikit.Size) []string {
	body := []string{
		"",
		"Pick the target disk(s) with SPACE.",
		tuikit.Dim + "  1 disk   = single: btrfs system partition, the rest an LVM data pool" + tuikit.Reset,
		tuikit.Dim + "  2+ disks = mirror: md RAID1 system on the first two, either one boots" + tuikit.Reset,
		"",
	}
	body = append(body, m.diskTable(inner.Cols, true)...)
	body = append(body, "", tuikit.Cyan+m.layoutName()+tuikit.Reset)
	if m.errmsg != "" {
		body = append(body, tuikit.Red+m.errmsg+tuikit.Reset)
	}
	return body
}

// field draws a text input as a reverse-video block showing the tail of value and a cursor.
func field(value string, width int) string {
	shown := value + "_"
	if tuikit.Width(shown) > width-2 {
		shown = shown[len(shown)-(width-2):]
	}
	return tuikit.Reverse + " " + tuikit.Pad(shown, width-2) + " " + tuikit.Reset
}

func (m *tuiModel) viewNetwork(inner tuikit.Size) []string {
	return []string{
		"",
		"The node gets its address by DHCP. For a static address, quit and run",
		"`expanse install --config <file>` instead (see docs/INSTALL.md).",
		"",
		tuikit.Bold + "Hostname" + tuikit.Reset + tuikit.Dim + "  (blank = expanse-<node-id prefix>)" + tuikit.Reset,
		field(m.hostname, min(inner.Cols, 60)),
	}
}

func (m *tuiModel) viewSSH(inner tuikit.Size) []string {
	return []string{
		"",
		"Paste an SSH public key (one line), or gh:<username> to fetch it from GitHub.",
		"The installed node has no root password: this key is how you log in.",
		"",
		tuikit.Bold + "Public key" + tuikit.Reset,
		field(m.sshKey, inner.Cols),
	}
}

func (m *tuiModel) viewReview(inner tuikit.Size) []string {
	const lw = 10
	body := []string{
		"",
		tuikit.KV("Hostname", orDefault(m.hostname, "expanse-<node-id prefix>"), lw),
		tuikit.KV("Network", "DHCP", lw),
		tuikit.KV("SSH key", orDefault(m.sshKey, "(none)"), lw),
	}
	if m.sshKey == "" {
		body = append(body, tuikit.KV("", tuikit.Red+"no SSH key: you will not be able to log in to this node"+tuikit.Reset, lw))
	}
	body = append(body, tuikit.KV("Layout", m.layoutShort(), lw), "",
		tuikit.Red+tuikit.Bold+"WILL DESTROY all data on:"+tuikit.Reset)
	for i, d := range m.disks {
		if m.picked[i] {
			body = append(body, tuikit.Red+"    "+tuikit.Strip(strings.Join([]string{d.Path, d.Size, d.Model, diskContents(d)}, "  "))+tuikit.Reset)
		}
	}
	body = append(body, "",
		"The root filesystem is wiped on EVERY REBOOT. All state lives in /persist.", "",
		tuikit.Bold+"Type INSTALL (all caps) to proceed"+tuikit.Reset,
		field(m.typed, 12))
	return body
}

func (m *tuiModel) viewProgress(inner tuikit.Size, g tuikit.Glyphs) []string {
	stages := install.Stages()
	idx, desc := 0, "" // 1-based stage number; 0 until the first "==>" line
	for i, st := range stages {
		if st.Name == m.stage {
			idx, desc = i+1, st.Desc
		}
	}
	elapsed := m.now.Sub(m.started).Round(time.Second)
	body := []string{
		"",
		tuikit.KV("Phase", fmt.Sprintf("%s%s%s (%d of %d)  %s", tuikit.Bold+tuikit.Cyan, orDefault(m.stage, "starting"), tuikit.Reset,
			idx, len(stages), tuikit.Dim+desc+tuikit.Reset), 8),
		tuikit.KV("", fmt.Sprintf("%s  elapsed %02d:%02d", tuikit.ProgressBar(max(idx-1, 0), len(stages), min(inner.Cols-30, 40), g),
			int(elapsed.Minutes()), int(elapsed.Seconds())%60), 8),
		tuikit.Rule("Log  " + installLogPath),
	}
	tail := max(inner.Rows-len(body), 0)
	start := max(len(m.log)-tail, 0)
	for _, l := range m.log[start:] {
		body = append(body, tuikit.Dim+l+tuikit.Reset)
	}
	return body
}

func (m *tuiModel) viewDone(inner tuikit.Size) []string {
	if m.errmsg != "" {
		body := []string{"", tuikit.Red + tuikit.Bold + "INSTALL FAILED" + tuikit.Reset, ""}
		body = append(body, tuikit.Wrap(tuikit.Red+m.errmsg+tuikit.Reset, inner.Cols)...)
		return append(body, "",
			"Full output: "+installLogPath,
			"Fix the problem and run `expanse install --tui` again: the installer is",
			"idempotent and safe to retry.")
	}
	const lw = 10
	ip := "<node-ip>"
	if len(m.ips) > 0 {
		ip = m.ips[0]
	}
	body := []string{
		"", tuikit.Green + tuikit.Bold + "INSTALL COMPLETE" + tuikit.Reset, "",
		tuikit.KV("Node ID", orDefault(m.nodeID, "(unreadable)"), lw),
		tuikit.KV("Hostname", orDefault(m.hostname, "expanse-"+firstN(m.nodeID, 6)), lw),
	}
	body = append(body, addressLines(m.ips, lw)...)
	body = append(body,
		tuikit.Rule("Next steps"),
		"1. Remove the installer media and reboot.",
		"2. Log in with your SSH key:   "+tuikit.Cyan+"ssh root@"+ip+tuikit.Reset,
		"3. Open the web UI:            "+tuikit.Cyan+"https://"+ip+":8443"+tuikit.Reset,
		"   The admin password is logged once at first start:  "+tuikit.Cyan+"journalctl -u expansed"+tuikit.Reset,
		"4. Form a one-node cluster (more nodes can join later):",
		"   "+tuikit.Cyan+"systemctl stop expansed && expanse cluster init --expect 1 \\"+tuikit.Reset,
		"   "+tuikit.Cyan+"  && systemctl start expansed"+tuikit.Reset,
	)
	return body
}

// addressLines lists the machine's addresses one per line (at most three), with a DHCP caveat.
func addressLines(ips []string, lw int) []string {
	lines := []string{tuikit.KV("Addresses", "(none yet)", lw)}
	for i, ip := range ips {
		if i == 3 {
			lines = append(lines, tuikit.KV("", fmt.Sprintf("and %d more", len(ips)-3), lw))
			break
		}
		label := ""
		if i == 0 {
			label, lines = "Addresses", nil
		}
		lines = append(lines, tuikit.KV(label, ip, lw))
	}
	return append(lines, tuikit.KV("", tuikit.Dim+"DHCP: they may change after the reboot"+tuikit.Reset, lw))
}

func firstN(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
