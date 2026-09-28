package tuikit

import (
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unsafe"
)

type termios struct {
	Iflag, Oflag, Cflag, Lflag uint32
	Line                       uint8
	Cc                         [32]uint8
	Ispeed, Ospeed             uint32
}

type winsize struct{ Rows, Cols, X, Y uint16 }

func ioctl(fd, req uintptr, p unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(p)); errno != 0 {
		return errno
	}
	return nil
}

// Term drives a raw-mode terminal: keys arrive on a channel, frames go out in one write.
type Term struct {
	in, out *os.File
	saved   termios
	keys    chan string
	resize  chan os.Signal
	glyphs  Glyphs
}

// Open puts the terminal (stdin/stdout) into raw mode: no echo, no line buffering, no
// signal keys, so Ctrl-C reaches the model instead of killing the process.
func Open() (*Term, error) {
	t := &Term{in: os.Stdin, out: os.Stdout, keys: make(chan string, 64), resize: make(chan os.Signal, 1)}
	if err := ioctl(t.in.Fd(), syscall.TCGETS, unsafe.Pointer(&t.saved)); err != nil {
		return nil, err
	}
	raw := t.saved
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := ioctl(t.in.Fd(), syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	t.glyphs = DefaultGlyphs()
	signal.Notify(t.resize, syscall.SIGWINCH)
	go t.readKeys()
	_, _ = t.out.WriteString("\x1b[?25l\x1b[2J") // hide the cursor; clear once
	return t, nil
}

// DefaultGlyphs is Unicode unless EXPANSE_TUI_ASCII is set or TERM is dumb.
func DefaultGlyphs() Glyphs {
	if os.Getenv("EXPANSE_TUI_ASCII") != "" || os.Getenv("TERM") == "dumb" {
		return ASCII
	}
	return Unicode
}

func (t *Term) readKeys() {
	buf := make([]byte, 4096)
	for {
		n, err := t.in.Read(buf)
		if err != nil {
			close(t.keys)
			return
		}
		for _, k := range ParseKeys(buf[:n]) {
			t.keys <- k
		}
	}
}

// Keys delivers parsed keys; the channel closes when stdin does.
func (t *Term) Keys() <-chan string { return t.keys }

// Resized fires once per terminal resize (SIGWINCH).
func (t *Term) Resized() <-chan os.Signal { return t.resize }

// Glyphs is the box-drawing set chosen for this terminal.
func (t *Term) Glyphs() Glyphs { return t.glyphs }

// Size queries the terminal size, falling back to the VT's 80x25.
func (t *Term) Size() Size {
	var ws winsize
	if err := ioctl(t.out.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil || ws.Cols == 0 || ws.Rows == 0 {
		return VT
	}
	return Size{Cols: int(ws.Cols), Rows: int(ws.Rows)}
}

// Draw repaints the whole screen from lines in a single write, without a flashing clear:
// each line is cleared to its end and anything below the last line is erased.
func (t *Term) Draw(lines []string) {
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(l + "\x1b[K")
	}
	b.WriteString("\x1b[J")
	_, _ = t.out.WriteString(b.String())
}

// Close restores the terminal: cooked mode, cursor shown, screen cleared.
func (t *Term) Close() {
	signal.Stop(t.resize)
	_, _ = t.out.WriteString(Reset + "\x1b[2J\x1b[H\x1b[?25h")
	_ = ioctl(t.in.Fd(), syscall.TCSETS, unsafe.Pointer(&t.saved))
}
