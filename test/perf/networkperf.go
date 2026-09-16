// Network perf measurements for §6 (Phase 05): L7 LB throughput and
// added latency against a direct-to-backend baseline, L4 throughput
// ratio, and DNS authoritative latency — all in-process (loopback),
// standing in for wrk/iperf3 which the go test environment does not
// ship. proxy_rss_bytes and vip_failover_ms stay VM-measured (see the
// test file's notes and budgets.yaml).
package perf

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	expdns "github.com/expanse/expanse/internal/network/dns"
	"github.com/expanse/expanse/internal/proxy"
	"github.com/miekg/dns"
)

// streamBytes is the payload size for one L4 throughput stream (64 MiB).
const streamBytes = 64 << 20

// dnsQueries is the authoritative-latency sample count (§6: 10k).
const dnsQueries = 10000

// perfDuration is the load window. Default 10 s locally; RUN_PERF=1
// uses the spec's 30 s (PERF_DURATION overrides either).
func perfDuration() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("PERF_DURATION")); err == nil && d > 0 {
		return d
	}
	if os.Getenv("RUN_PERF") != "" {
		return 30 * time.Second
	}
	return 10 * time.Second
}

// fixedTable is a fixed-snapshot proxy.TableSource.
type fixedTable struct{ t *proxy.Table }

func (f *fixedTable) Table() *proxy.Table { return f.t }

// l7Fixture is one L7 instance over 3 loopback backends, plus the
// direct addresses for the baseline.
type l7Fixture struct {
	lbAddr  string   // host:port of the L7 listener
	direct  []string // "host:port" of each backend
	closeFn func()
}

// startL7Fixture builds 3 backend HTTP servers (the nginx-replica
// stand-in: a tiny 200 with a fixed body) and one L7 listener routing
// web.default.expanse.local → the three backends round-robin.
func startL7Fixture() (*l7Fixture, error) {
	var backends []string
	var ports []int
	var closers []func()
	for range 3 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("hello"))
		})}
		go func() { _ = srv.Serve(ln) }()
		ap := ln.Addr().(*net.TCPAddr)
		backends = append(backends, ap.AddrPort().String())
		ports = append(ports, ap.Port)
		closers = append(closers, func() { _ = srv.Close() })
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	lbPort := ln.Addr().(*net.TCPAddr).Port
	host := "127.0.0.1"
	svc := &proxy.Service{
		Key:        "default/web",
		Namespace:  "default",
		Name:       "web",
		Port:       int32(lbPort),
		TargetPort: 8080,
	}
	for i := range backends {
		svc.Backends = append(svc.Backends, proxy.Backend{
			ReplicaIndex: int32(i),
			NodeID:       fmt.Sprintf("n%d", i+1),
			Healthy:      true,
		})
	}
	tbl := &proxy.Table{Services: map[string]*proxy.Service{svc.Key: svc}}

	l7 := &proxy.L7{
		Pool:   &fixedTable{t: tbl},
		Routes: proxy.RoutesFrom(&fixedTable{t: tbl}),
		Resolve: func(b proxy.Backend, _ int32) string {
			return net.JoinHostPort(host, fmt.Sprint(ports[b.ReplicaIndex]))
		},
	}
	hs := &http.Server{Handler: l7.Handler()}
	go func() { _ = hs.Serve(ln) }()
	closers = append(closers, func() { _ = hs.Close() })

	return &l7Fixture{
		lbAddr: net.JoinHostPort(host, fmt.Sprint(lbPort)),
		direct: backends,
		closeFn: func() {
			for _, c := range closers {
				c()
			}
		},
	}, nil
}

// measureL7Load runs the throughput window: 100 concurrent keep-alive
// GET loops through the L7 listener. Returns achieved requests/second.
func measureL7Load(lbAddr string, dur time.Duration) (float64, error) {
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        128,
			MaxIdleConnsPerHost: 128,
		},
	}
	var wg sync.WaitGroup
	var total atomic.Int64
	var runErr error
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	start := time.Now()
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+lbAddr+"/", nil)
				if err != nil {
					return
				}
				req.Host = "web.default.expanse.local"
				resp, err := client.Do(req)
				if err != nil {
					// The window closing kills in-flight requests —
					// that's not a load error.
					if ctx.Err() != nil {
						return
					}
					mu.Lock()
					if runErr == nil {
						runErr = err
					}
					mu.Unlock()
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				total.Add(1)
			}
		}()
	}
	wg.Wait()
	if runErr != nil {
		return 0, runErr
	}
	return float64(total.Load()) / time.Since(start).Seconds(), nil
}

// measureLatencyP99 sends n sequential requests via fn and returns the
// p99 latency in µs.
func measureLatencyP99(n int, fn func() error) float64 {
	lat := make([]float64, 0, n)
	for range n {
		start := time.Now()
		_ = fn()
		lat = append(lat, float64(time.Since(start).Nanoseconds())/1000)
	}
	sort.Float64s(lat)
	return lat[len(lat)*99/100]
}

// measureL7AddedLatency compares p99 through the LB against the
// direct-to-backend baseline measured in the same process and window.
// Returns (lbP99µs, directP99µs, addedµs).
func measureL7AddedLatency(f *l7Fixture, n int) (float64, float64, float64) {
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(addr, host string) func() error {
		return func() error {
			req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
			if host != "" {
				req.Host = host
			}
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			return resp.Body.Close()
		}
	}
	// Warm connections before measuring.
	for range 50 {
		_ = get(f.lbAddr, "web.default.expanse.local")()
		_ = get(f.direct[0], "")()
	}
	lbP99 := measureLatencyP99(n, get(f.lbAddr, "web.default.expanse.local"))
	directP99 := measureLatencyP99(n, get(f.direct[0], ""))
	return lbP99, directP99, lbP99 - directP99
}

// measureL4ThroughputRatio streams a fixed payload through the L4
// proxy and directly to the backend sink; returns proxy/direct
// throughput ratio (§6: ≥ 0.8 of "line rate", where the same-process
// direct loopback stream is the line-rate reference).
func measureL4ThroughputRatio() (float64, error) {
	body := make([]byte, streamBytes)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(streamBytes))
		_, _ = w.Write(body)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	ap := ln.Addr().(*net.TCPAddr)
	host := "127.0.0.1"

	l4ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l4 := &proxy.L4{
		Pool: &fixedTable{t: &proxy.Table{Services: map[string]*proxy.Service{
			"default/sink": {
				Key: "default/sink", Namespace: "default", Name: "sink",
				Port: int32(l4ln.Addr().(*net.TCPAddr).Port), TargetPort: int32(ap.Port),
				Backends: []proxy.Backend{{ReplicaIndex: 0, NodeID: "n1", Healthy: true}},
			},
		}}},
		Key:     "default/sink",
		Resolve: func(b proxy.Backend, targetPort int32) string { return net.JoinHostPort(host, fmt.Sprint(targetPort)) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = l4.Serve(ctx, l4ln) }()
	defer l4.Close()
	time.Sleep(100 * time.Millisecond) // listener settle

	throughput := func(addr string) (float64, error) {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return 0, err
		}
		defer conn.Close()
		if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: sink\r\n\r\n"); err != nil {
			return 0, err
		}
		buf := make([]byte, 64*1024)
		var got int64
		for got < streamBytes {
			n, rerr := conn.Read(buf)
			got += int64(n)
			if rerr != nil {
				break
			}
		}
		return float64(got) / time.Since(start).Seconds(), nil
	}
	// Warm, then measure direct vs proxy.
	_, _ = throughput(ln.Addr().String())
	_, _ = throughput(l4ln.Addr().String())
	direct, err := throughput(ln.Addr().String())
	if err != nil {
		return 0, err
	}
	proxied, err := throughput(l4ln.Addr().String())
	if err != nil {
		return 0, err
	}
	if direct <= 0 {
		return 0, fmt.Errorf("direct throughput measured 0")
	}
	return proxied / direct, nil
}

// measureDNSLatencyP99 answers 10k authoritative A queries against the
// §4.4 server's Answer path and returns the p99 in µs.
func measureDNSLatencyP99() float64 {
	srv := expdns.NewServer()
	srv.SetZone([]expdns.Record{
		{Name: "web.default.expanse.local.", Type: "A", TTL: 30, Data: "192.168.1.100"},
	})
	lat := make([]float64, 0, dnsQueries)
	for range dnsQueries {
		m := &dns.Msg{}
		m.SetQuestion("web.default.expanse.local.", dns.TypeA)
		start := time.Now()
		_ = srv.Answer(m)
		lat = append(lat, float64(time.Since(start).Nanoseconds())/1000)
	}
	sort.Float64s(lat)
	return lat[len(lat)*99/100]
}
