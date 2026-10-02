package health

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/expanse/expanse/proto"
)

// scriptProbe answers from results in order, then repeats the last one.
type scriptProbe struct {
	results []bool
	calls   int
}

func (s *scriptProbe) Probe(ctx context.Context, t Target) Result {
	i := min(s.calls, len(s.results)-1)
	s.calls++
	return Result{OK: s.results[i], Detail: "scripted"}
}

// watchdogHarness runs a Watchdog on a fake clock, recording restarts and publications.
type watchdogHarness struct {
	w         *Watchdog
	now       time.Time
	restarts  int
	published []LivenessRecord
	sleeps    []time.Duration
}

func newWatchdogHarness(probe Prober, p *pb.HealthProbe) *watchdogHarness {
	h := &watchdogHarness{now: time.Unix(1000, 0)}
	h.w = &Watchdog{
		Prober:      probe,
		Probe:       p,
		MaxRestarts: 2,
		Backoff:     func(n int) time.Duration { return time.Duration(n) * time.Minute },
		StableAfter: 10 * time.Minute,
		Restart:     func(context.Context) error { h.restarts++; return nil },
		Publish: func(_ context.Context, r LivenessRecord) error {
			h.published = append(h.published, r)
			return nil
		},
		Now: func() time.Time { return h.now },
		Sleep: func(ctx context.Context, d time.Duration) bool {
			h.sleeps = append(h.sleeps, d)
			h.now = h.now.Add(d)
			return ctx.Err() == nil && h.now.Before(time.Unix(1000, 0).Add(24*time.Hour))
		},
	}
	return h
}

func failingAfter(n int) []bool {
	r := make([]bool, n+1)
	for i := range n {
		r[i] = true
	}
	return r
}

func TestWatchdogRestartsThenGivesUp(t *testing.T) {
	p := &pb.HealthProbe{PeriodSeconds: 5, FailureThreshold: 3, InitialDelaySeconds: 30}
	h := newWatchdogHarness(&scriptProbe{results: []bool{false}}, p)
	h.w.Run(context.Background())

	if h.restarts != 2 {
		t.Fatalf("restarts = %d, want 2", h.restarts)
	}
	if n := len(h.published); n != 3 {
		t.Fatalf("published %d records, want 3: %+v", n, h.published)
	}
	for i, r := range h.published[:2] {
		if r.Failed || r.Restarts != i+1 {
			t.Fatalf("record %d = %+v, want restarts=%d, not failed", i, r, i+1)
		}
	}
	if last := h.published[2]; !last.Failed || last.Restarts != 2 || last.Detail != "scripted" {
		t.Fatalf("last record = %+v, want failed after 2 restarts", last)
	}
	// Initial delay, 2 periods to reach the threshold, then backoff + initial delay after each restart.
	want := []time.Duration{30 * time.Second, 5 * time.Second, 5 * time.Second, time.Minute + 30*time.Second}
	for i, d := range want {
		if h.sleeps[i] != d {
			t.Fatalf("sleep %d = %v, want %v (all: %v)", i, h.sleeps[i], d, h.sleeps)
		}
	}
}

func TestWatchdogIgnoresFailuresBelowThreshold(t *testing.T) {
	p := &pb.HealthProbe{PeriodSeconds: 5, FailureThreshold: 3}
	h := newWatchdogHarness(&scriptProbe{results: []bool{false, false, true, false, false, true}}, p)
	h.w.Run(context.Background())
	if h.restarts != 0 || len(h.published) != 0 {
		t.Fatalf("restarts = %d, published = %v; want none", h.restarts, h.published)
	}
}

func TestWatchdogStableReplicaEarnsBackItsRestarts(t *testing.T) {
	// Fail, restart, pass for 15 minutes, then fail for good: the budget of 2 starts over.
	script := append([]bool{false}, failingAfter(15*60/5)...)
	h := newWatchdogHarness(&scriptProbe{results: script}, &pb.HealthProbe{PeriodSeconds: 5})
	h.w.Run(context.Background())
	if h.restarts != 3 {
		t.Fatalf("restarts = %d, want 3 (1 before the stable spell, 2 after)", h.restarts)
	}
	if last := h.published[len(h.published)-1]; !last.Failed || last.Restarts != 3 {
		t.Fatalf("last record = %+v, want failed with 3 restarts in all", last)
	}
}

func TestWatchdogCountsAFailedRestart(t *testing.T) {
	h := newWatchdogHarness(&scriptProbe{results: []bool{false}}, &pb.HealthProbe{PeriodSeconds: 5})
	h.w.Restart = func(context.Context) error { h.restarts++; return errors.New("job failed") }
	h.w.Run(context.Background())
	if h.restarts != 2 || !h.published[len(h.published)-1].Failed {
		t.Fatalf("restarts = %d, records = %+v; want 2 then failed", h.restarts, h.published)
	}
}
