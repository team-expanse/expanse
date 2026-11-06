package console

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// LineWriter passes only whole lines to w, ending a partial one left idle, so other writers to
// the same stream (the runner's own log lines in the journal) never continue a guest's line.
type LineWriter struct {
	w    io.Writer
	idle time.Duration

	mu      sync.Mutex
	partial []byte
	timer   *time.Timer
}

// NewLineWriter writes to w, ending a partial line after idle without more output.
func NewLineWriter(w io.Writer, idle time.Duration) *LineWriter {
	return &LineWriter{w: w, idle: idle}
}

// Write keeps p's unfinished tail back until its line ends or goes idle.
func (l *LineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.partial = append(l.partial, p...)
	if i := bytes.LastIndexByte(l.partial, '\n'); i >= 0 {
		if _, err := l.w.Write(l.partial[:i+1]); err != nil {
			return 0, err
		}
		l.partial = append(l.partial[:0:0], l.partial[i+1:]...)
	}
	switch {
	case len(l.partial) == 0 && l.timer != nil:
		l.timer.Stop()
	case len(l.partial) > 0 && l.timer == nil:
		l.timer = time.AfterFunc(l.idle, l.Flush)
	case len(l.partial) > 0:
		l.timer.Reset(l.idle)
	}
	return len(p), nil
}

// Flush ends and writes a partial line, if any.
func (l *LineWriter) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.partial) == 0 {
		return
	}
	_, _ = l.w.Write(append(l.partial, '\n'))
	l.partial = nil
}
