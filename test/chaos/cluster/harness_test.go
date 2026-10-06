package chaos

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/store"
)

// ackedWriter writes n keys through the leader in batches and returns a stopped Writer that acked them.
func ackedWriter(t *testing.T, h *Harness, n int) *Writer {
	t.Helper()
	w := &Writer{h: h, acked: make(map[store.Key]acked), stopped: make(chan struct{})}
	close(w.stopped)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for start := 0; start < n; start += 500 {
		var ops []store.Op
		for i := start; i < min(start+500, n); i++ {
			ops = append(ops, store.Op{Kind: store.OpPut, Key: store.Key(fmt.Sprintf("/chaos/k%d", i)), Value: fmt.Appendf(nil, "v%d", i)})
		}
		rev, err := h.Leader().Txn(ctx, ops)
		if err != nil {
			t.Fatalf("seed writes: %v", err)
		}
		for _, op := range ops {
			w.acked[op.Key] = acked{val: string(op.Value), rev: rev}
		}
	}
	return w
}

func TestVerifyAckedChecksAFullLengthRunWithinItsBudget(t *testing.T) {
	h := NewHarness(t, 3)
	w := ackedWriter(t, h, 50000)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.VerifyAcked(ctx); err != nil {
		t.Fatalf("VerifyAcked: %v", err)
	}
}

func TestVerifyAckedReportsALostWrite(t *testing.T) {
	h := NewHarness(t, 3)
	w := ackedWriter(t, h, 10)
	w.acked["/chaos/lost"] = acked{val: "v", rev: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := w.VerifyAcked(ctx)
	if err == nil || !strings.Contains(err.Error(), "/chaos/lost: acked write missing") {
		t.Fatalf("VerifyAcked = %v, want the lost key reported missing", err)
	}
}
