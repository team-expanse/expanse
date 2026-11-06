package console

import (
	"testing"
	"time"
)

func TestLineWriterPassesOnlyWholeLines(t *testing.T) {
	out := &syncBuffer{}
	w := NewLineWriter(out, time.Hour)
	_, _ = w.Write([]byte("[  OK  ] Started"))
	if got := out.String(); got != "" {
		t.Fatalf("a partial line was written: %q", got)
	}
	_, _ = w.Write([]byte(" foo\r\nlogin: "))
	if got := out.String(); got != "[  OK  ] Started foo\r\n" {
		t.Fatalf("got %q, want the finished line only", got)
	}
}

// A guest that leaves a line unfinished (a prompt, a terminal query) has it ended after idle,
// so the next writer to the same journal stream starts on a line of its own.
func TestLineWriterEndsAnIdlePartialLine(t *testing.T) {
	out := &syncBuffer{}
	w := NewLineWriter(out, 20*time.Millisecond)
	_, _ = w.Write([]byte("\x1bP+q6E616D65\x1b\\"))
	eventually(t, "the partial line to be ended", func() bool { return out.String() == "\x1bP+q6E616D65\x1b\\\n" })
	_, _ = w.Write([]byte("next\n"))
	eventually(t, "the next line", func() bool { return out.String() == "\x1bP+q6E616D65\x1b\\\nnext\n" })
}

func TestLineWriterFlushEndsThePartialLine(t *testing.T) {
	out := &syncBuffer{}
	w := NewLineWriter(out, time.Hour)
	_, _ = w.Write([]byte("Power down"))
	w.Flush()
	if got := out.String(); got != "Power down\n" {
		t.Fatalf("got %q after Flush", got)
	}
	w.Flush()
	if got := out.String(); got != "Power down\n" {
		t.Fatalf("an empty Flush wrote %q", got)
	}
}
