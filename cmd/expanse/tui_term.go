package main

import (
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"

	"github.com/expanse/expanse/internal/install"
)

// tuiOptions carry the install flags the TUI passes through to install.Run.
type tuiOptions struct {
	TargetFlake       string
	SkipSystemInstall bool
}

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

// rawMode turns off echo, line buffering and signal keys, returning a func that restores the terminal.
func rawMode() (func(), error) {
	var saved termios
	if err := ioctl(os.Stdin.Fd(), syscall.TCGETS, &saved); err != nil {
		return nil, err
	}
	raw := saved
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := ioctl(os.Stdin.Fd(), syscall.TCSETS, &raw); err != nil {
		return nil, err
	}
	return func() { _ = ioctl(os.Stdin.Fd(), syscall.TCSETS, &saved) }, nil
}

// parseKeys turns one read's bytes into keys: named keys, or runs of printable text (a paste is one run).
func parseKeys(b []byte) []string {
	var keys []string
	text := ""
	flush := func() {
		if text != "" {
			keys = append(keys, text)
			text = ""
		}
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c >= 0x20 && c != 0x7f {
			text += string(c)
			continue
		}
		flush()
		switch c {
		case '\r', '\n':
			if c == '\r' && i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
			keys = append(keys, "enter")
		case 0x7f, 0x08:
			keys = append(keys, "backspace")
		case 0x03, 0x04:
			keys = append(keys, "ctrl-c")
		case '\t':
			keys = append(keys, "tab")
		case 0x1b:
			i = skipEscape(b, i, &keys)
		}
	}
	flush()
	return keys
}

// skipEscape consumes the escape sequence at b[i], recording up/down, and returns its last index.
func skipEscape(b []byte, i int, keys *[]string) int {
	if i+1 >= len(b) || b[i+1] != '[' {
		return i
	}
	j := i + 2
	for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
		j++
	}
	if j < len(b) {
		switch b[j] {
		case 'A':
			*keys = append(*keys, "up")
		case 'B':
			*keys = append(*keys, "down")
		}
	}
	return j
}

func draw(m *tuiModel) { fmt.Print("\x1b[2J\x1b[H" + m.View()) }

// runTUI drives the interactive installer on the controlling terminal.
func runTUI(opts tuiOptions) error {
	restore, err := rawMode()
	if err != nil {
		return fmt.Errorf("put terminal in raw mode: %w", err)
	}
	defer restore()

	disks, err := install.DetectDisks()
	m := newTUIModel(disks)
	if err != nil {
		m.errmsg = err.Error()
	}
	buf := make([]byte, 4096)
	for {
		draw(m)
		if m.screen == screenProgress {
			m.finish(runTUIInstall(m, opts))
			continue
		}
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return nil
		}
		for _, k := range parseKeys(buf[:n]) {
			if m.Update(k) {
				fmt.Print("\x1b[2J\x1b[H")
				return nil
			}
		}
	}
}

// runTUIInstall runs the install from the model's choices, echoing all output to the screen and installLogPath.
func runTUIInstall(m *tuiModel, opts tuiOptions) (string, error) {
	logFile, err := os.Create(installLogPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = logFile.Close() }()
	out := io.MultiWriter(os.Stdout, logFile) // raw mode keeps output processing, so \n still ends a line

	cfgFile, err := os.CreateTemp("", "expanse-install-*.yaml")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(cfgFile.Name()) }()
	if err := m.installConfig().WriteYAML(cfgFile.Name()); err != nil {
		return "", err
	}
	err = install.Run(install.Options{
		ConfigPath:        cfgFile.Name(),
		Force:             true,
		TargetFlake:       opts.TargetFlake,
		SkipSystemInstall: opts.SkipSystemInstall,
		Out:               out,
		Logger:            func(f string, a ...any) { _, _ = fmt.Fprintf(out, f+"\n", a...) },
	})
	if err != nil {
		return "", err
	}
	id, err := install.LoadIdentity("/mnt/persist/expanse/identity")
	if err != nil {
		return "", nil // installed; the done screen says the ID is unreadable
	}
	return id.NodeID.String(), nil
}
