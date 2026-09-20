package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func TestDescribeLeaseNamesTheHolderAndItsExpiry(t *testing.T) {
	ctx := context.Background()
	st, err := boltstore.New(t.TempDir() + "/l.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if got := describeLease(ctx, st, "vol-x"); !strings.Contains(got, "no lease record") {
		t.Fatalf("unheld lease described as %q", got)
	}
	h, err := lease.NewManager(st, "n2").TryAcquire(ctx, "vol-x", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Abandon()
	got := describeLease(ctx, st, "vol-x")
	if !strings.Contains(got, "n2") || !strings.Contains(got, "expires in") {
		t.Fatalf("described as %q: want the holder and time to expiry", got)
	}
}
