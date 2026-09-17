package transport

import (
	"sync"
	"time"
)

// Throttle is a token-bucket byte rate limiter for resync traffic
// (§4.3 bandwidth control: default 100 MiB/s, configurable). It only
// ever wraps resync chunk sends — foreground writes are never
// rate-limited.
type Throttle struct {
	ratePerSec float64 // refill rate, bytes per second
	burst      float64 // bucket capacity

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// NewThrottle creates a limiter allowing bytesPerSec with a burst of
// max(bytesPerSec/2, minBurst) — enough to keep chunked sends pipelined
// without letting resync exceed its rate budget in aggregate.
func NewThrottle(bytesPerSec int64) *Throttle {
	const minBurst = 64 << 10
	burst := float64(bytesPerSec) / 2
	if burst < minBurst {
		burst = minBurst
	}
	if burst > float64(bytesPerSec) {
		burst = float64(bytesPerSec)
	}
	return &Throttle{
		ratePerSec: float64(bytesPerSec),
		burst:      burst,
		tokens:     burst,
		last:       time.Now(),
	}
}

// Wait blocks until n bytes of budget are available.
func (t *Throttle) Wait(n int) {
	if n <= 0 || t.ratePerSec <= 0 {
		return
	}
	for {
		t.mu.Lock()
		now := time.Now()
		t.tokens += now.Sub(t.last).Seconds() * t.ratePerSec
		if t.tokens > t.burst {
			t.tokens = t.burst
		}
		t.last = now
		if t.tokens >= float64(n) {
			t.tokens -= float64(n)
			t.mu.Unlock()
			return
		}
		// Sleep for the shortfall at the refill rate.
		missing := (float64(n) - t.tokens) / t.ratePerSec
		t.mu.Unlock()
		time.Sleep(time.Duration(missing * float64(time.Second)))
	}
}
