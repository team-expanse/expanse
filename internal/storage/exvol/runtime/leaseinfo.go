package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/store"
)

// describeLease says who holds a lease and for how much longer, so a refused
// acquisition in the logs explains itself instead of only saying "another holder".
func describeLease(ctx context.Context, st store.Store, name string) string {
	l, err := lease.Inspect(ctx, st, name)
	switch {
	case err != nil:
		return "lease unreadable: " + err.Error()
	case l == nil:
		return "no lease record"
	}
	left := time.Until(l.ExpiresAt).Round(10 * time.Millisecond)
	if left < 0 {
		return fmt.Sprintf("held by %s, expired %v ago", l.Holder, -left)
	}
	return fmt.Sprintf("held by %s, expires in %v", l.Holder, left)
}
