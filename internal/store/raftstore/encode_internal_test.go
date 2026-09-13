package raftstore

import (
	"testing"

	"github.com/expanse/expanse/internal/errors"
)

// TestApplyErrorShape pins the wrapped error constructor used by the
// forward server's op application.
func TestApplyErrorShape(t *testing.T) {
	err := applyError(errors.KindConflict, "test-op", "boom")
	if !errors.Is(err, errors.KindConflict) {
		t.Errorf("kind lost: %v", err)
	}
	if err.Error() == "" {
		t.Error("empty message")
	}
}
