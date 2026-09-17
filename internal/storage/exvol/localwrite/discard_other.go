//go:build !linux

package localwrite

// Discard is a no-op on platforms without FALLOC_FL_PUNCH_HOLE.
func (w *Writer) Discard(off, length int64) error { return nil }
