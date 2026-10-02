// Package health implements the agent-side health probing (PHASE04.md
// §5.4): the Prober interface with tcp/http/exec/none implementations,
// and a Runner that invokes the prober every periodSeconds and publishes
// results to /blocks/<ns>/<name>/status/replicas/<i> — but ONLY on
// transition, plus an unconditional heartbeat every HeartbeatInterval
// (§5.4 write-amplification rule: a 10-replica cluster with 5 s probes
// must not generate 2 Raft writes/s of pure noise).
//
// Linearizability: probe writes go through the plain store write path.
// Stale reads are explicit OPT-IN per the store contract; this package
// never opts in, so store.AssertNoStaleReads on this directory passes by
// construction — there is no skip-list to register on. Probe results are
// linearizable Raft writes like any other status write.
package health

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"time"

	"github.com/expanse/expanse/internal/blocks/lifecycle"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// HeartbeatInterval is the unconditional write period even when nothing
// changes (§5.4: "plus a heartbeat every 60 s").
const HeartbeatInterval = 60 * time.Second

// Result is one probe outcome.
type Result struct {
	OK      bool   // pass/fail
	Detail  string // human detail ("connection refused", "HTTP 503")
	Err     string // probe infrastructure error, if any
	At      time.Time
	Latency time.Duration
}

// Target describes what to probe.
type Target struct {
	// Address for tcp/http probes (host:port).
	Address string
	// Path is the HTTP path (http probes; empty = "/").
	Path string
	// Command is the exec probe command (runs in the block's namespace —
	// the agent unit already carries the namespace context).
	Command []string
	// Timeout bounds one probe; default 5s.
	Timeout time.Duration
}

func (t Target) timeout() time.Duration {
	if t.Timeout <= 0 {
		return 5 * time.Second
	}
	return t.Timeout
}

// Prober probes one target once.
type Prober interface {
	Probe(ctx context.Context, t Target) Result
}

// ---- Implementations ----

// TCPProber dials with a timeout (§5.4).
type TCPProber struct{}

// Probe implements Prober.
func (TCPProber) Probe(ctx context.Context, t Target) Result {
	start := time.Now()
	d := net.Dialer{Timeout: t.timeout()}
	conn, err := d.DialContext(ctx, "tcp", t.Address)
	if err != nil {
		return Result{OK: false, Detail: err.Error(), At: start, Latency: time.Since(start)}
	}
	conn.Close()
	return Result{OK: true, Detail: "connected", At: start, Latency: time.Since(start)}
}

// HTTPProber GETs and passes on 200–399, following NO redirects (§5.4).
type HTTPProber struct {
	// Client is overridable for tests; default: no redirect following.
	Client *http.Client
}

// Probe implements Prober.
func (p HTTPProber) Probe(ctx context.Context, t Target) Result {
	start := time.Now()
	client := p.Client
	if client == nil {
		client = &http.Client{
			// No redirect following: a redirect is not a pass (§5.4).
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: t.timeout(),
		}
	}
	path := t.Path
	if path == "" {
		path = "/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+t.Address+path, nil)
	if err != nil {
		return Result{OK: false, Detail: err.Error(), Err: err.Error(), At: start, Latency: time.Since(start)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{OK: false, Detail: err.Error(), Err: err.Error(), At: start, Latency: time.Since(start)}
	}
	defer resp.Body.Close()
	ok := resp.StatusCode >= 200 && resp.StatusCode <= 399
	return Result{
		OK: ok, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode),
		At: start, Latency: time.Since(start),
	}
}

// ExecProber runs the target command with a timeout (§5.4).
type ExecProber struct{}

// Probe implements Prober.
func (ExecProber) Probe(ctx context.Context, t Target) Result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, t.timeout())
	defer cancel()
	if len(t.Command) == 0 {
		return Result{OK: false, Detail: "empty exec command", At: start}
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, t.Command[0], t.Command[1:]...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	detail := out.String()
	if len(detail) > 200 {
		detail = detail[:200]
	}
	if err != nil {
		return Result{OK: false, Detail: detail, Err: err.Error(), At: start, Latency: time.Since(start)}
	}
	return Result{OK: true, Detail: detail, At: start, Latency: time.Since(start)}
}

// NoneProber always passes (PROBE_NONE).
type NoneProber struct{}

// Probe implements Prober.
func (NoneProber) Probe(ctx context.Context, t Target) Result {
	return Result{OK: true, Detail: "none", At: time.Now()}
}

// For returns a Prober for a HealthProbe's type. Unspecified → none.
func For(t pb.ProbeType) Prober {
	switch t {
	case pb.ProbeType_PROBE_TCP:
		return TCPProber{}
	case pb.ProbeType_PROBE_HTTP:
		return HTTPProber{}
	case pb.ProbeType_PROBE_EXEC:
		return ExecProber{}
	default:
		return NoneProber{}
	}
}

// Record is what gets persisted at /blocks/<ns>/<name>/status/replicas/<i>.
// JSON: a tiny leaf record with no proto message; the /status/ subtree
// never feeds generation snapshots, so encoding choice is local.
type Record struct {
	OK        bool      `json:"ok"`
	Detail    string    `json:"detail"`
	At        time.Time `json:"at"`
	LatencyNs int64     `json:"latencyNs"`
	// Heartbeat marks unconditional periodic writes that carry unchanged
	// state (§5.4); consumers treat them identically.
	Heartbeat bool `json:"heartbeat,omitempty"`
	// Node is the node that probed; Retire removes only its own node's record.
	Node string `json:"node,omitempty"`
}

// thresholdCounter implements success/failureThreshold counting (§5.4):
// a transition fires only after N consecutive opposite results.
type thresholdCounter struct {
	successThreshold int // consecutive passes to go healthy (default 1)
	failureThreshold int // consecutive fails to go unhealthy (default 1)
	streak           int // consecutive same-result count (negative = failing)
	healthy          bool
	seen             bool // false until the first result seeds state
	initDone         bool // thresholds copied from the probe spec
}

func (c *thresholdCounter) observe(ok bool) (transitioned bool) {
	st := c.successThreshold
	if st <= 0 {
		st = 1
	}
	ft := c.failureThreshold
	if ft <= 0 {
		ft = 1
	}
	if ok {
		if c.streak > 0 {
			c.streak++
		} else {
			c.streak = 1
		}
		if c.seen && !c.healthy && c.streak >= st {
			c.healthy, c.streak, transitioned = true, 0, true
		}
	} else {
		if c.streak < 0 {
			c.streak--
		} else {
			c.streak = -1
		}
		if c.seen && c.healthy && -c.streak >= ft {
			c.healthy, c.streak, transitioned = false, 0, true
		}
	}
	if !c.seen {
		c.seen, c.healthy, c.streak = true, ok, 0 // seed: no write on first result
	}
	return transitioned
}

// Publisher persists one probe record. The production implementation
// writes the store linearizably (no stale reads); tests inject counters.
type Publisher func(ctx context.Context, rec Record) error

func replicaKey(blockKey store.Key, replicaIndex int32) store.Key {
	return store.Key(fmt.Sprintf("%s/status/replicas/%d", blockKey, replicaIndex))
}

// StorePublisher publishes rec, stamped with node, at the replica's status key with a
// read-revision + CAS txn (Phase03 pattern: OpCheck guards concurrent
// status writers).
func StorePublisher(st store.Store, blockKey store.Key, replicaIndex int32, node string) Publisher {
	key := replicaKey(blockKey, replicaIndex)
	return func(ctx context.Context, rec Record) error {
		rec.Node = node
		b, err := json.Marshal(rec)
		if err != nil {
			return errors.Wrap(err, errors.KindInternal, "health.publish", "marshal record")
		}
		e, err := st.Get(ctx, key)
		if err != nil && !errors.Is(err, errors.KindNotFound) {
			return errors.Wrap(err, errors.KindInternal, "health.publish", "read status")
		}
		var ops []store.Op
		if e != nil {
			ops = append(ops, store.Op{Kind: store.OpCheck, Key: key, Expect: e.Revision})
		}
		ops = append(ops, store.Op{Kind: store.OpPut, Key: key, Value: b})
		if _, err := st.Txn(ctx, ops); err != nil {
			return errors.Wrap(err, errors.KindUnavailable, "health.publish", "write status")
		}
		return nil
	}
}

// Passing reports whether node's latest probe of the replica passed; no record is not a pass.
func Passing(ctx context.Context, st store.Store, blockKey store.Key, replicaIndex int32, node string) (bool, error) {
	e, err := st.Get(ctx, replicaKey(blockKey, replicaIndex))
	if errors.Is(err, errors.KindNotFound) {
		return false, nil
	}
	if err != nil {
		return false, errors.Wrap(err, errors.KindInternal, "health.passing", "read status")
	}
	var rec Record
	if json.Unmarshal(e.Value, &rec) != nil {
		return false, nil
	}
	return rec.OK && rec.Node == node, nil
}

func livenessKey(blockKey store.Key, replicaIndex int32) store.Key {
	return store.Key(fmt.Sprintf("%s/status/liveness/%d", blockKey, replicaIndex))
}

// LivenessPublisher writes a replica's liveness record, stamped with node.
func LivenessPublisher(st store.Store, blockKey store.Key, replicaIndex int32, node string) func(context.Context, LivenessRecord) error {
	key := livenessKey(blockKey, replicaIndex)
	return func(ctx context.Context, rec LivenessRecord) error {
		rec.Node = node
		b, err := json.Marshal(rec)
		if err != nil {
			return errors.Wrap(err, errors.KindInternal, "health.publishLiveness", "marshal record")
		}
		if _, err := st.Put(ctx, key, b); err != nil {
			return errors.Wrap(err, errors.KindUnavailable, "health.publishLiveness", "write status")
		}
		return nil
	}
}

// LivenessFailedOn returns node's record if node gave up on the replica; nil when it has not,
// or when the record is missing, from another node or unreadable (unknown is not unhealthy).
func LivenessFailedOn(ctx context.Context, st store.Store, blockKey store.Key, replicaIndex int32, node string) (*LivenessRecord, error) {
	var rec LivenessRecord
	ok, err := readOwn(ctx, st, livenessKey(blockKey, replicaIndex), node, &rec, &rec.Node)
	if err != nil || !ok || !rec.Failed {
		return nil, err
	}
	return &rec, nil
}

// RecordsOf returns node's readiness and liveness records for the replica; nil where node has none.
func RecordsOf(ctx context.Context, st store.Store, blockKey store.Key, replicaIndex int32, node string) (*Record, *LivenessRecord, error) {
	var ready Record
	var live LivenessRecord
	okReady, err := readOwn(ctx, st, replicaKey(blockKey, replicaIndex), node, &ready, &ready.Node)
	if err != nil {
		return nil, nil, err
	}
	okLive, err := readOwn(ctx, st, livenessKey(blockKey, replicaIndex), node, &live, &live.Node)
	if err != nil {
		return nil, nil, err
	}
	var r *Record
	var l *LivenessRecord
	if okReady {
		r = &ready
	}
	if okLive {
		l = &live
	}
	return r, l, nil
}

// readOwn decodes key into rec and reports whether node wrote it; missing or garbled is false.
func readOwn(ctx context.Context, st store.Store, key store.Key, node string, rec any, writer *string) (bool, error) {
	e, err := st.Get(ctx, key)
	if errors.Is(err, errors.KindNotFound) {
		return false, nil
	}
	if err != nil {
		return false, errors.Wrap(err, errors.KindInternal, "health.readOwn", "read status")
	}
	return json.Unmarshal(e.Value, rec) == nil && *writer == node, nil
}

// Retire deletes the replica's record if node wrote it, so a replica gone from node leaves no
// stale verdict, yet the record of a replica that moved to another node survives.
func Retire(ctx context.Context, st store.Store, blockKey store.Key, replicaIndex int32, node string) error {
	return retire(ctx, st, replicaKey(blockKey, replicaIndex), node)
}

// RetireLiveness is Retire for the replica's liveness record.
func RetireLiveness(ctx context.Context, st store.Store, blockKey store.Key, replicaIndex int32, node string) error {
	return retire(ctx, st, livenessKey(blockKey, replicaIndex), node)
}

func retire(ctx context.Context, st store.Store, key store.Key, node string) error {
	e, err := st.Get(ctx, key)
	if errors.Is(err, errors.KindNotFound) {
		return nil
	}
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, "health.retire", "read status")
	}
	var rec struct {
		Node string `json:"node"`
	}
	if json.Unmarshal(e.Value, &rec) != nil || rec.Node != node {
		return nil
	}
	ops := []store.Op{{Kind: store.OpCheck, Key: key, Expect: e.Revision}, {Kind: store.OpDelete, Key: key}}
	if _, err := st.Txn(ctx, ops); err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "health.retire", "delete status")
	}
	return nil
}

// Runner drives probing for one replica and publishes with the §5.4
// write-amplification rule: write on transition, heartbeat otherwise.
type Runner struct {
	// Publisher receives records (production: StorePublisher).
	Publisher Publisher
	// Prober per probe kind.
	Prober Prober
	// Target to probe.
	Target Target
	// Probe supplies thresholds and the default period.
	Probe *pb.HealthProbe
	// Period between probes; falls back to Probe.PeriodSeconds, then 5s.
	Period time.Duration
	// Heartbeat overrides HeartbeatInterval (tests).
	Heartbeat time.Duration
	// InitialDelay before the first probe; falls back to Probe.InitialDelaySeconds.
	InitialDelay time.Duration
	// Liveness marks a liveness prober: failures emit EventLivenessFail
	// (→ Failed via retry/reschedule) instead of EventReadinessFail.
	Liveness bool
	// OnEvent fires lifecycle Events (T09) on transitions.
	OnEvent func(lifecycle.Event)
	// Now is the clock (fake in tests). Defaults to time.Now.
	Now func() time.Time

	st   thresholdCounter
	last time.Time // last WRITE (transition or heartbeat)
}

// Run probes until ctx is done, after the initial delay, then every period.
func (r *Runner) Run(ctx context.Context) {
	period := r.period()
	beat := r.Heartbeat
	if beat <= 0 {
		beat = HeartbeatInterval
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(r.initialDelay()):
	}
	r.ProbeOnce(ctx)
	tick := time.NewTicker(period)
	defer tick.Stop()
	beatTick := time.NewTicker(beat)
	defer beatTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.ProbeOnce(ctx)
		case <-beatTick.C:
			if r.now().Sub(r.last) >= beat {
				// Unconditional heartbeat carrying unchanged state (§5.4).
				r.write(ctx, Record{OK: r.st.healthy, Detail: "heartbeat", At: r.now(), Heartbeat: true})
			}
		}
	}
}

// ProbeOnce performs one probe cycle: probe, publish the first result (a replica that never
// passes must say so), and on a threshold-crossing transition publish + fire the lifecycle
// event. Exposed so tests can drive the state machine without wall-clock races.
func (r *Runner) ProbeOnce(ctx context.Context) {
	r.init()
	res := r.Prober.Probe(ctx, r.Target)
	first := !r.st.seen
	transitioned := r.st.observe(res.OK)
	if first || transitioned {
		r.write(ctx, Record{
			OK: res.OK, Detail: res.Detail, At: r.now(),
			LatencyNs: int64(res.Latency),
		})
	}
	if transitioned {
		r.fire(res.OK)
	}
}

// write publishes and stamps the last-write clock.
func (r *Runner) write(ctx context.Context, rec Record) {
	if r.Publisher != nil {
		_ = r.Publisher(ctx, rec) //nolint: probe writes retry next tick; never fatal
	}
	r.last = r.now()
}

func (r *Runner) fire(passed bool) {
	if r.OnEvent == nil {
		return
	}
	switch {
	case r.Liveness && !passed:
		r.OnEvent(lifecycle.EventLivenessFail)
	case !r.Liveness && passed:
		r.OnEvent(lifecycle.EventReadinessPass)
	case !r.Liveness && !passed:
		r.OnEvent(lifecycle.EventReadinessFail)
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// init seeds the threshold counter from r.Probe exactly once.
func (r *Runner) init() {
	if r.st.seen || r.st.initDone {
		return
	}
	r.st.successThreshold = int(r.Probe.GetSuccessThreshold())
	r.st.failureThreshold = int(r.Probe.GetFailureThreshold())
	r.st.initDone = true
}

func (r *Runner) initialDelay() time.Duration {
	if r.InitialDelay > 0 {
		return r.InitialDelay
	}
	return time.Duration(r.Probe.GetInitialDelaySeconds()) * time.Second
}

func (r *Runner) period() time.Duration {
	if r.Period > 0 {
		return r.Period
	}
	if s := r.Probe.GetPeriodSeconds(); s > 0 {
		return time.Duration(s) * time.Second
	}
	return 5 * time.Second
}
