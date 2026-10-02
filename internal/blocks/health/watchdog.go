package health

import (
	"context"
	"time"

	pb "github.com/expanse/expanse/proto"
)

// LivenessRecord is what a node publishes at /blocks/<ns>/<name>/status/liveness/<i>.
type LivenessRecord struct {
	Restarts int       `json:"restarts"`
	Failed   bool      `json:"failed,omitempty"` // the node gave up restarting the replica
	Detail   string    `json:"detail"`
	At       time.Time `json:"at"`
	Node     string    `json:"node,omitempty"`
}

// Watchdog restarts a replica whose liveness probe fails failureThreshold times in a row, and
// declares it failed once MaxRestarts restarts have not kept it passing for StableAfter.
type Watchdog struct {
	Prober  Prober
	Target  Target
	Probe   *pb.HealthProbe
	Restart func(ctx context.Context) error
	Publish func(ctx context.Context, rec LivenessRecord) error
	// MaxRestarts, Backoff and StableAfter default to 5, 10 s doubling to 5 min, and 10 min.
	MaxRestarts int
	Backoff     func(n int) time.Duration
	StableAfter time.Duration
	// Now and Sleep are the clock (fakes in tests); Sleep reports false once ctx ends.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) bool
}

// Run probes until ctx ends or the replica is declared failed.
func (w *Watchdog) Run(ctx context.Context) {
	w.defaults()
	period := time.Duration(max(w.Probe.GetPeriodSeconds(), 0)) * time.Second
	if period == 0 {
		period = 5 * time.Second
	}
	threshold := int(max(w.Probe.GetFailureThreshold(), 1))
	delay := time.Duration(max(w.Probe.GetInitialDelaySeconds(), 0)) * time.Second

	total, recent, fails := 0, 0, 0
	var passingSince time.Time
	wait := delay
	for w.Sleep(ctx, wait) {
		wait = period
		res := w.Prober.Probe(ctx, w.Target)
		if res.OK {
			fails = 0
			if passingSince.IsZero() {
				passingSince = w.Now()
			} else if w.Now().Sub(passingSince) >= w.StableAfter {
				recent = 0
			}
			continue
		}
		passingSince = time.Time{}
		if fails++; fails < threshold {
			continue
		}
		fails = 0
		rec := LivenessRecord{Restarts: total, Detail: res.Detail, At: w.Now()}
		if recent >= w.MaxRestarts {
			rec.Failed = true
			_ = w.Publish(ctx, rec) //nolint: the node's next agent start probes afresh
			return
		}
		total, recent = total+1, recent+1
		_ = w.Restart(ctx) // a failed restart still spends one of the budget
		rec.Restarts = total
		_ = w.Publish(ctx, rec)
		wait = w.Backoff(recent) + delay
	}
}

func (w *Watchdog) defaults() {
	if w.MaxRestarts <= 0 {
		w.MaxRestarts = 5
	}
	if w.Backoff == nil {
		w.Backoff = func(n int) time.Duration { return min(10*time.Second<<(n-1), 5*time.Minute) }
	}
	if w.StableAfter <= 0 {
		w.StableAfter = 10 * time.Minute
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.Sleep == nil {
		w.Sleep = func(ctx context.Context, d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-t.C:
				return true
			}
		}
	}
}
