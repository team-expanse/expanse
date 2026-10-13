package proxy

import (
	"fmt"
	"sync"
	"time"
)

// DefaultEjectFor is how long a backend that failed to connect is skipped.
const DefaultEjectFor = 10 * time.Second

// ejector is passive outlier ejection: a backend that failed to connect is
// skipped for For, so a frozen node costs one dial timeout, not one per request.
// The zero value is ready to use.
type ejector struct {
	For time.Duration    // default DefaultEjectFor
	now func() time.Time // default time.Now

	until sync.Map // backend key → time.Time
}

func ejectKey(b Backend) string { return fmt.Sprintf("%s/%d", b.NodeID, b.ReplicaIndex) }

func (e *ejector) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func (e *ejector) eject(b Backend) {
	d := e.For
	if d <= 0 {
		d = DefaultEjectFor
	}
	e.until.Store(ejectKey(b), e.clock().Add(d))
}

func (e *ejector) readmit(b Backend) { e.until.Delete(ejectKey(b)) }

// live drops ejected backends, failing open to all of them when none would remain.
func (e *ejector) live(cands []Backend) []Backend {
	now := e.clock()
	out := make([]Backend, 0, len(cands))
	for _, b := range cands {
		if t, ok := e.until.Load(ejectKey(b)); ok && now.Before(t.(time.Time)) {
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return cands
	}
	return out
}
