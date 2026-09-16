package vip

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/errors"
)

// Holder constants from spec §4.2.
const (
	// LeaseTTL is the VIP lease TTL (renewed at TTL/3 by the lease
	// package). Lease loss closes Done within ~TTL/3 + 2 s < TTL.
	// 10 s (not the lease package's 15 s default) bounds failover to
	// G5.4's ≤ 15 s measured from the external client: takeover waits
	// out the expiry (uniform in [TTL/3, TTL] after holder death), plus
	// slippage, election if the leader died, and announcement — worst
	// case ~14 s at TTL 10, ~20 s at TTL 15 (measured 16.7–21.3 s in
	// the m3-demo/net-vip-failover VM tests).
	LeaseTTL = 10 * time.Second
	// AcquireRetry is the candidate retry cadence: every 2 s while a
	// ready replica exists locally.
	AcquireRetry = 2 * time.Second
	// AnnounceCount is the number of gratuitous ARP (IPv4) / unsolicited
	// NA (IPv6) packets sent on acquisition.
	AnnounceCount = 3
)

// LeaseName is the lease key for a VIP address.
func LeaseName(addr netip.Addr) string {
	return "vip:" + addr.String()
}

// Candidate is a node that could hold a VIP: its ID and how many ready
// replicas of the VIP's block it currently hosts.
type Candidate struct {
	NodeID        string
	ReadyReplicas int
}

// PickPreferred implements the §4.2 preference rule: among candidates,
// most ready replicas wins; ties go to the lowest node ID. Prevents
// pointless flapping after failover. Returns "" for no candidates.
func PickPreferred(cands []Candidate) string {
	var best *Candidate
	for i := range cands {
		c := &cands[i]
		if c.ReadyReplicas <= 0 {
			continue
		}
		if best == nil ||
			c.ReadyReplicas > best.ReadyReplicas ||
			(c.ReadyReplicas == best.ReadyReplicas && c.NodeID < best.NodeID) {
			best = c
		}
	}
	if best == nil {
		return ""
	}
	return best.NodeID
}

// ShouldAttempt reports whether self should try to acquire the VIP
// lease now. A candidate attempts when it has a ready replica and
// either the lease is free/expired or self is preferred over the
// recorded holder (§4.2 preference — a less-preferred candidate waits
// instead of grabbing, which is what causes flapping).
func ShouldAttempt(self string, holderLease lease.Lease, cands []Candidate) bool {
	if PickPreferred(cands) != self {
		return false // no ready replica here, or a better candidate exists
	}
	if holderLease.Holder == "" || holderLease.Holder == self {
		return true
	}
	// Only attempt if self is preferred over the current holder. The
	// holder keeps the lease unless someone strictly better exists.
	holderReady := 0
	for _, c := range cands {
		if c.NodeID == holderLease.Holder {
			holderReady = c.ReadyReplicas
		}
	}
	selfReady := 0
	for _, c := range cands {
		if c.NodeID == self {
			selfReady = c.ReadyReplicas
		}
	}
	return selfReady > holderReady
}

// ConnTracker drops in-flight connections on demotion. The LB
// implementation (T09+) registers tracked connections here.
type ConnTracker interface {
	CloseAll()
}

// Seams are the platform edges of the holder, injectable for tests.
// In production (linux.go) these wrap netlink, raw sockets for the
// gratuitous ARP/NA, and the LB listener.
type Seams struct {
	// AddAddr assigns the VIP to the interface (netlink.AddrAdd).
	AddAddr func(p netip.Prefix) error
	// DelAddr removes the VIP from the interface (netlink.AddrDel).
	DelAddr func(p netip.Prefix) error
	// Announce sends n gratuitous ARP (IPv4) / unsolicited NA (IPv6).
	Announce func(p netip.Prefix, n int) error
	// Listen starts the LB listener bound to the VIP; the returned
	// closer must stop serving when closed. T09 replaces the stub.
	Listen func(p netip.Prefix) (io.Closer, error)
	// Tracker drops tracked connections. May be nil.
	Tracker ConnTracker
}

// HolderConfig configures one VIP holder loop.
type HolderConfig struct {
	Leases *lease.Manager
	Self   string // node ID
	VIP    netip.Prefix
	Block  string // owning block ref
	Cands  func() []Candidate
	Seams  Seams
	Retry  time.Duration // default AcquireRetry
	TTL    time.Duration // default LeaseTTL
	// OnAcquired / OnLost fire after a full becomeHolder and at the
	// START of onLeaseLost respectively. The agent publishes/clears
	// the /network/vips/<addr>/holder record through them (mesh
	// AllowedIPs read that record — publish only once actually serving).
	OnAcquired func(p netip.Prefix)
	OnLost     func(p netip.Prefix)
	NowFunc    func() time.Time
	// Logger is optional; nil disables VIP lease logging.
	Logger *slog.Logger
}

// Holder runs the §4.2 acquisition flow for one VIP: while a ready
// replica of the block exists locally, try to acquire the lease every
// retry; on success add the address, announce, and listen; on lease
// loss run the shutdown sequence — DelAddr FIRST, then listener close,
// then connection drop, never reversed (spec: removing the address
// first means clients see silent packet loss and retry, which is what
// TCP is for; stopping the listener first means connection-refused
// from an IP about to move).
type Holder struct {
	cfg HolderConfig
}

func NewHolder(cfg HolderConfig) *Holder {
	if cfg.Retry <= 0 {
		cfg.Retry = AcquireRetry
	}
	if cfg.TTL <= 0 {
		cfg.TTL = LeaseTTL
	}
	return &Holder{cfg: cfg}
}

// holderState is the currently-attached serving state.
type holderState struct {
	held     *lease.Held
	listener io.Closer
}

// Run drives the acquisition loop until ctx is done. It returns when
// the context is canceled or the block loses all ready replicas
// (release, then exit — the caller restarts Run if replicas return).
func (h *Holder) log(level func(string, ...any), msg string, args ...any) {
	if h.cfg.Logger != nil {
		level(msg, append([]any{"vip", h.cfg.VIP.Addr(), "node", h.cfg.Self}, args...)...)
	}
}

func (h *Holder) Run(ctx context.Context) error {
	retry := h.cfg.Retry
	timer := time.NewTimer(0)
	defer timer.Stop()
	waitC := timer.C

	var state *holderState
	defer func() {
		if state != nil {
			h.release(ctx, state)
		}
	}()

	for {
		// Acquisition attempt: only while a ready replica exists locally.
		wait := retry
		cands := h.cfg.Cands()
		if state == nil && cands != nil {
			// Read the recorded lease so the preference decision sees
			// the real holder. A dead holder has no ready-replica count
			// in the candidate set (holderReady = 0), so a survivor
			// becomes eligible the moment the fence expires — the lease
			// expiry itself is the liveness proof; no separate failure
			// detection is needed.
			l, _, gerr := h.cfg.Leases.Inspect(ctx, LeaseName(h.cfg.VIP.Addr()))
			should := gerr == nil && ShouldAttempt(h.cfg.Self, l, cands)
			if !should && gerr == nil && l.Holder != "" && !l.ExpiresAt.IsZero() {
				// Expired lease overrides preference: a holder that has
				// failed to renew for a full TTL is dead by definition
				// (renewal runs at TTL/3). Any live candidate may race
				// for it via the takeover CAS — the fence still prevents
				// split brain, and the winner is whoever commits first.
				should = time.Now().After(l.ExpiresAt)
			}
			if should {
				h.log(h.cfg.Logger.Debug, "acquire attempt")
				if hld, err := h.cfg.Leases.TryAcquire(ctx, LeaseName(h.cfg.VIP.Addr()), h.cfg.TTL); err == nil {
					st, err := h.becomeHolder(hld)
					if err != nil {
						h.log(h.cfg.Logger.Warn, "becomeHolder failed", "err", err)
						hld.Abandon()
						return err
					}
					h.log(h.cfg.Logger.Info, "acquired VIP lease")
					h.onAcquired()
					state = st
				} else if err == lease.ErrNotAcquired {
					h.log(h.cfg.Logger.Debug, "lease held elsewhere")
					// Preferred-but-held: retry exactly at the recorded
					// expiry (+ slippage) instead of the next blind tick
					// — failover is bounded by the lease fence, not by
					// fence + retry interval. The wait can also shorten
					// or lengthen relative to the retry interval.
					if d := time.Until(l.ExpiresAt) + 100*time.Millisecond; d > time.Second/10 {
						wait = d
					}
				}
				// Other store-level errors resolve to "retry on the
				// next tick".
			}
		}

		if state != nil {
			select {
			case <-state.held.Done():
				h.log(h.cfg.Logger.Warn, "lost VIP lease")
				h.onLeaseLost(state)
				state = nil
				// Immediately eligible for re-acquisition on the next
				// tick (e.g. we remain the preferred candidate).
			default:
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-waitC:
			timer.Reset(wait)
		}
	}
}

func (h *Holder) becomeHolder(hld *lease.Held) (*holderState, error) {
	s := h.cfg.Seams
	if err := s.AddAddr(h.cfg.VIP); err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "vip", "add address")
	}
	if err := s.Announce(h.cfg.VIP, AnnounceCount); err != nil {
		_ = s.DelAddr(h.cfg.VIP)
		return nil, errors.Wrap(err, errors.KindUnavailable, "vip", "gratuitous announce")
	}
	lc, err := s.Listen(h.cfg.VIP)
	if err != nil {
		_ = s.DelAddr(h.cfg.VIP)
		return nil, errors.Wrap(err, errors.KindUnavailable, "vip", "listen")
	}
	return &holderState{held: hld, listener: lc}, nil
}

func (h *Holder) onAcquired() {
	if h.cfg.OnAcquired != nil {
		h.cfg.OnAcquired(h.cfg.VIP)
	}
}

// onLeaseLost is the dangerous part (§4.2, verbatim order):
//
//  1. DelAddr  — FIRST. Packets stop being delivered; clients retry.
//  2. listener.Close — then stop serving.
//  3. Tracker.CloseAll — then drop in-flight connections.
func (h *Holder) onLeaseLost(state *holderState) {
	if h.cfg.OnLost != nil {
		h.cfg.OnLost(h.cfg.VIP) // clear the holder record FIRST (we may already be stale)
	}
	s := h.cfg.Seams
	_ = s.DelAddr(h.cfg.VIP) // FIRST
	if state.listener != nil {
		_ = state.listener.Close() // then
	}
	if s.Tracker != nil {
		s.Tracker.CloseAll() // then
	}
	// Tidy the lease bookkeeping: the record expires on its own; we do
	// not delete it (another node may take over sooner via expiry).
	state.held.Abandon()
}

func (h *Holder) release(ctx context.Context, state *holderState) {
	h.onLeaseLost(state)
}
