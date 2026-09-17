//go:build linux

package localwrite

import "golang.org/x/sys/unix"

// Discard punches a hole in the file/device (trim support, §4.4).
// Kept size: the range reads back as zeros afterwards.
func (w *Writer) Discard(off, length int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return unix.Fallocate(int(w.f.Fd()), unix.FALLOC_FL_KEEP_SIZE|unix.FALLOC_FL_PUNCH_HOLE, off, length)
}
