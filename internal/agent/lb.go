package agent

// LB wiring (§4.3): one proxy.Pool per agent watches /blocks/ and
// feeds every VIP holder's L4 listener. This replaces the T07
// local-only splice: the VIP holder now load-balances across ALL
// healthy backends cluster-wide, with the pool's atomic table swap
// keeping in-flight connections on a consistent snapshot.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/proxy"
	"github.com/expanse/expanse/internal/store"
)

// nodeAddrTTL bounds the node-address cache (the resolve seam runs per
// accepted connection; re-reading the join record every time would put
// a store round-trip on the hot path).
const nodeAddrTTL = 30 * time.Second

type nodeAddr struct {
	ip string
	at time.Time
}

// closerFunc adapts a func to io.Closer.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// initLB creates the backend pool (called once per Agent, before any
// holder starts — see initVIP).
func (a *Agent) initLB() {
	a.lbPool = proxy.NewPool(a.store)
}

// lbPoolLoop runs the backend pool, re-listing when the watch channel
// closes (internal/store's never-silent-drop overflow contract). Runs
// until ctx is done.
func (a *Agent) lbPoolLoop(ctx context.Context) {
	for {
		err := a.lbPool.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		a.logger.Warn("lb pool watch ended; re-listing", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// lbResolve maps a backend to its dial address: the node's advertise
// address from its join record (/nodes/<id>, JSON NodeRecord) plus the
// service's target port. Cached nodeAddrTTL per node.
func (a *Agent) lbResolve(b proxy.Backend, port int32) string {
	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()
	if v, ok := a.nodeAddr[b.NodeID]; ok && v.ip != "" && time.Since(v.at) < nodeAddrTTL {
		return net.JoinHostPort(v.ip, strconv.Itoa(int(port)))
	}
	ip := a.lookupNodeIP(b.NodeID)
	a.nodeAddr[b.NodeID] = nodeAddr{ip: ip, at: time.Now()}
	if ip == "" {
		return ""
	}
	return net.JoinHostPort(ip, strconv.Itoa(int(port)))
}

// lookupNodeIP reads one node's join record and extracts a routable
// host (raft advertise addr first, then the API addr).
func (a *Agent) lookupNodeIP(nodeID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e, err := a.store.Get(store.WithStale(ctx), store.Key("/nodes/"+nodeID))
	if err != nil {
		return ""
	}
	var rec struct {
		RaftAddr string `json:"raft_addr"`
		APIAddr  string `json:"api_addr"`
	}
	if json.Unmarshal(e.Value, &rec) != nil {
		return ""
	}
	for _, cand := range []string{rec.RaftAddr, rec.APIAddr} {
		host, _, err := net.SplitHostPort(cand)
		if err == nil && host != "" && host != "0.0.0.0" {
			return host
		}
	}
	return ""
}

// lbListen builds the listener seam one VIP holder serves: bind
// VIP:exposedPort and load-balance to the pool's healthy backends for
// svcKey. The returned Closer stops the accept loop; established
// connections drain per L4.DrainTimeout (the holder releases the lease
// only when fenced out, so this is the failover path, not steady
// state).
func (a *Agent) lbListen(p netip.Prefix, exposedPort int32, svcKey string) (io.Closer, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(p.Addr().String(), strconv.Itoa(int(exposedPort))))
	if err != nil {
		return nil, err
	}
	l4 := &proxy.L4{
		Pool: a.lbPool,
		Key:  svcKey,
		Mode: proxy.RoundRobin, // §4.3 default algorithm
		Resolve: func(b proxy.Backend, port int32) string {
			return a.lbResolve(b, port)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		if err := l4.Serve(ctx, ln); err != nil {
			a.logger.Warn("lb listener exited", "vip", p.Addr(), "err", err)
		}
	}()
	return closerFunc(func() error {
		l4.Close()
		_ = ln.Close()
		cancel()
		return nil
	}), nil
}

// compile-time guard: the Agent's store satisfies the pool Source.
var _ proxy.Source = aStore(nil)

type aStore store.Store

var _ = sync.Mutex{}
