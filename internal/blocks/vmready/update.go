package vmready

import "strings"

// updateNote marks a status whose VM runs an older QEMU than its node now has.
const updateNote = " (QEMU update pending)"

// WithUpdateNote appends the note when pending; WithAddresses goes after it.
func WithUpdateNote(status string, pending bool) string {
	if !pending {
		return status
	}
	return status + updateNote
}

// SplitUpdateNote undoes WithUpdateNote.
func SplitUpdateNote(text string) (string, bool) {
	return strings.CutSuffix(text, updateNote)
}
