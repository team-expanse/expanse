package chaosstorage

import (
	"errors"
	"testing"
)

func TestClearError(t *testing.T) {
	clear := []string{
		"exvol.device.write: EIO: volume lease lost",
		"exvol.write: exvol.write not quorum-durable: 1 of 3 replicas (quorum 2)",
		"exvol.primary.write: EIO: primary fenced: a write reached no quorum",
		"not primary for vol-x",
		"exvol.device.write: device closed",
	}
	for _, m := range clear {
		if !ClearError(errors.New(m)) {
			t.Errorf("%q should be a clear error", m)
		}
	}
	for _, m := range []string{"unexpected EOF", "index out of range", ""} {
		if ClearError(errors.New(m)) {
			t.Errorf("%q must not count as a clear error", m)
		}
	}
	if !ClearError(nil) {
		t.Error("nil is trivially clear")
	}
}
