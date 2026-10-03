package volume

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/drbd"
)

// Lease is the authority to be a volume's primary. *lease.Held satisfies it.
type Lease interface {
	Valid() bool
	Done() <-chan struct{}
}

// Consumer stops every user of a volume's device, in practice by unmounting it.
// Release must be safe to repeat and succeed when nothing uses the device.
type Consumer interface {
	Release(ctx context.Context, res string) error
}

type verb int

const (
	wait verb = iota
	promote
	forcePromote
	demote
)

func (v verb) String() string {
	return [...]string{"wait", "promote", "forcePromote", "demote"}[v]
}

// decide is the whole promotion policy: primary needs the lease and, with three
// or more replicas, DRBD quorum. Quorum lost while primary changes nothing, since
// on-no-quorum=io-error already fails the I/O; only the lease demotes.
func decide(st *drbd.Status, held, initial bool) verb {
	switch {
	case st.Role == drbd.RolePrimary && !held:
		return demote
	case st.Role != drbd.RoleSecondary || !held || len(st.Volumes) == 0 || !st.HasQuorum():
		return wait
	case initial && fresh(st):
		return forcePromote
	}
	return promote
}

// fresh reports that every replica is connected and none holds data, the only
// state in which forcing a promotion cannot discard anything.
func fresh(st *drbd.Status) bool {
	for _, v := range st.Volumes {
		if v.DiskState != drbd.DiskInconsistent {
			return false
		}
	}
	for _, p := range st.Peers {
		if p.Connection == drbd.ConnConnected && isDiskless(p) {
			continue // a tiebreaker holds no data
		}
		if p.Connection != drbd.ConnConnected || len(p.Volumes) == 0 {
			return false
		}
		for _, v := range p.Volumes {
			if v.DiskState != drbd.DiskInconsistent {
				return false
			}
		}
	}
	return true
}

// Promoter keeps a volume primary on this node exactly as long as the node holds
// the volume's lease, and demotes it on every way out.
type Promoter struct {
	DRBD     drbd.DRBD
	Consumer Consumer // nil when nothing mounts the device
	// Poll is the recheck interval until primary; Settled applies once it is.
	Poll, Settled time.Duration
	// StepDownTimeout bounds how long a demotion is retried before it is an error.
	StepDownTimeout time.Duration
	Log             *slog.Logger
}

// HoldOptions describes one Hold call.
type HoldOptions struct {
	// Initial marks a volume that has never been primary. It permits `primary
	// --force`, and only while every replica is connected and Inconsistent.
	Initial bool
	// OnPrimary runs after each promotion; the caller records the volume as initialised.
	OnPrimary func()
}

func or(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

func (p *Promoter) poll() time.Duration    { return or(p.Poll, time.Second) }
func (p *Promoter) settled() time.Duration { return or(p.Settled, 5*time.Second) }

func (p *Promoter) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Hold makes res primary while l is valid and ctx is live. However it ends (lease
// lost, ctx cancelled for shutdown, re-election or delete) the device is released
// and demoted before it returns; a non-nil error means that did not finish.
func (p *Promoter) Hold(ctx context.Context, res string, l Lease, opt HoldOptions) error {
	h := &holding{Promoter: p, res: res, opt: opt}
	for l.Valid() && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-l.Done():
		case <-time.After(h.evaluate(ctx)):
		}
	}
	return p.stepDown(res)
}

// holding is one Hold call; lastErr keeps a refused promotion from flooding the log.
type holding struct {
	*Promoter
	res     string
	opt     HoldOptions
	lastErr string
}

// evaluate runs one promotion check and returns how long to wait before the next.
func (h *holding) evaluate(ctx context.Context) time.Duration {
	st, err := h.DRBD.Status(ctx, h.res)
	if err != nil {
		h.note(err)
		return h.poll()
	}
	switch decide(st, true, h.opt.Initial) {
	case promote:
		err = h.DRBD.Primary(ctx, h.res)
	case forcePromote:
		h.log().Warn("forcing initial promotion of a fresh volume", "vol", h.res)
		err = h.DRBD.ForcePrimary(ctx, h.res)
	default:
		return h.interval(st)
	}
	if err != nil {
		h.note(err)
		return h.poll()
	}
	h.lastErr = ""
	if h.opt.OnPrimary != nil {
		h.opt.OnPrimary()
	}
	return h.settled()
}

func (h *holding) interval(st *drbd.Status) time.Duration {
	if st.Role == drbd.RolePrimary {
		return h.settled()
	}
	return h.poll()
}

func (h *holding) note(err error) {
	if msg := err.Error(); msg != h.lastErr {
		h.lastErr = msg
		h.log().Warn("promotion not possible yet", "vol", h.res, "err", msg)
	}
}

// stepDown releases the consumer and demotes, retrying until both hold or the
// timeout passes. It runs on a fresh context so a cancelled ctx cannot skip it.
func (p *Promoter) stepDown(res string) error {
	ctx, cancel := context.WithTimeout(context.Background(), or(p.StepDownTimeout, 30*time.Second))
	defer cancel()
	for {
		err := p.stepDownOnce(ctx, res)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return experrors.Wrap(err, experrors.KindUnavailable, "volume.stepDown", "volume "+res+" could not be demoted: "+err.Error())
		case <-time.After(p.poll()):
		}
	}
}

// stepDownOnce always tries both halves: DRBD, not the consumer's report, is the
// judge of whether the device is still open.
func (p *Promoter) stepDownOnce(ctx context.Context, res string) error {
	var released error
	if p.Consumer != nil {
		released = p.Consumer.Release(ctx, res)
	}
	st, err := p.DRBD.Status(ctx, res)
	switch {
	case experrors.KindOf(err) == experrors.KindNotFound:
		return released
	case err != nil:
		return errors.Join(released, err)
	}
	if decide(st, false, false) == demote {
		err = p.DRBD.Secondary(ctx, res)
	}
	return errors.Join(released, err)
}

// Lead keeps res primary here for as long as this node can hold the volume's
// lease, re-acquiring it after any loss, and returns once ctx ends. The volume is
// demoted before the lease is given up. A demotion that fails leaves the record to
// expire, so nobody else is admitted while this node may still be writing; once it
// is free again, the node takes it back and keeps the volume it could not let go. A
// restarted node takes back its own live record (AcquireReclaiming) but first
// demotes whatever role the previous agent left behind.
func (p *Promoter) Lead(ctx context.Context, mgr *lease.Manager, name, res string, ttl time.Duration, opt HoldOptions) error {
	for {
		err := p.stepDown(res)
		switch {
		case ctx.Err() != nil:
		case err == nil:
			err = p.holdLease(ctx, mgr, name, res, ttl, opt)
		default:
			err = p.holdStuck(ctx, mgr, name, res, ttl, opt, err)
		}
		if ctx.Err() != nil {
			return err
		}
		p.log().Warn("leading interrupted", "vol", res, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(p.poll()):
		}
	}
}

// holdLease acquires the lease and holds the volume under it until either ends.
func (p *Promoter) holdLease(ctx context.Context, mgr *lease.Manager, name, res string, ttl time.Duration, opt HoldOptions) error {
	held, err := mgr.AcquireReclaiming(ctx, name, ttl)
	if err != nil {
		return unlessStopped(ctx, err)
	}
	err = p.Hold(ctx, res, held, opt)
	p.giveUp(mgr, held, err)
	return err
}

// holdStuck holds a volume that could not be demoted, but only when its lease is
// free now; otherwise it returns stepErr so the demotion is retried.
func (p *Promoter) holdStuck(ctx context.Context, mgr *lease.Manager, name, res string, ttl time.Duration, opt HoldOptions, stepErr error) error {
	tctx, cancel := context.WithTimeout(ctx, p.poll())
	held, err := mgr.AcquireReclaiming(tctx, name, ttl)
	cancel()
	if err != nil {
		return stepErr
	}
	err = p.Hold(ctx, res, held, opt)
	p.giveUp(mgr, held, err)
	return err
}

// unlessStopped drops an error caused by ctx ending: stopping is not a failure.
func unlessStopped(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		return err
	}
	return nil
}

// giveUp releases the lease after a clean demotion and abandons it otherwise.
func (p *Promoter) giveUp(mgr *lease.Manager, held *lease.Held, demoteErr error) {
	if demoteErr != nil {
		held.Abandon()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := mgr.Release(ctx, held); err != nil {
		p.log().Warn("lease release failed", "err", err)
	}
}
