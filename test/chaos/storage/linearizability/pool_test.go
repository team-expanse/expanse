package linearizability

import (
	"math/rand"
	"testing"
)

func TestSettledKeysAreThoseBelowTheHotWindow(t *testing.T) {
	p := newKeyPool(4, 2, 100)
	if got := p.settledKeys(0); got != 0 {
		t.Fatalf("nothing is finished yet, settledKeys = %d", got)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 400; i++ {
		if _, ok := p.pick(rng); !ok {
			break
		}
	}
	lowest := p.settledKeys(0)
	if lowest == 0 || lowest > p.usedKeys() {
		t.Fatalf("settledKeys = %d after heavy use (used %d)", lowest, p.usedKeys())
	}
	if got := p.settledKeys(3); got != max(lowest-3, 0) {
		t.Fatalf("margin of 3: got %d, want %d", got, max(lowest-3, 0))
	}
	if got := p.settledKeys(1 << 20); got != 0 {
		t.Fatalf("a margin larger than the run leaves nothing settled, got %d", got)
	}
}
