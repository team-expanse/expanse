// §6 network perf tests (Phase 05 T24). Gated behind RUN_PERF=1 like
// the rest of the perf suite; budgets live in test/perf/budgets.yaml
// (lb_rps, lb_added_latency_p99_us, dns_query_p99_us,
// l4_throughput_ratio; proxy_rss_bytes and vip_failover_ms are
// VM-only — see budgets.yaml for why).
//
// In-process stand-ins, documented: wrk → 100 concurrent keep-alive Go
// loops (same shape: 100 connections); iperf3 → a same-process direct
// loopback stream as the line-rate reference, with the budget on the
// proxy/direct RATIO (≥ 0.6) rather than absolute bytes/s, since
// loopback has no wire rate.
package perf

import (
	"context"
	"os"
	"testing"
	"time"
)

func skipNoPerf(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("perf: skipped in -short")
	}
	if os.Getenv("RUN_PERF") == "" {
		t.Skip("set RUN_PERF=1 to run performance budget checks")
	}
}

// TestL7LBPerf measures throughput through the L7 LB (budget lb_rps ≥
// 20k) and p99 added latency vs a direct-to-backend baseline measured
// in the same window (budget lb_added_latency_p99_us ≤ 1000).
func TestL7LBPerf(t *testing.T) {
	skipNoPerf(t)
	f, err := startL7Fixture()
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	defer f.closeFn()

	rps, err := measureL7Load(f.lbAddr, perfDuration())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Logf("L7 throughput: %.0f req/s (budget ≥ 20000)", rps)
	if rps < 20000 {
		t.Errorf("lb_rps violated: %.0f < 20000", rps)
	}

	lbP99, directP99, added := measureL7AddedLatency(f, 2000)
	t.Logf("L7 p99 %.0fµs, direct p99 %.0fµs, added %.0fµs (budget ≤ 1000)", lbP99, directP99, added)
	if added > 1000 {
		t.Errorf("lb_added_latency_p99_us violated: %.0f > 1000", added)
	}
}

// TestL4Throughput asserts the L4 proxy sustains ≥ 60% of the direct-loopback
// rate; loopback pays the proxy's extra kernel hop on the same CPUs, unlike a wire.
func TestL4Throughput(t *testing.T) {
	skipNoPerf(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_ = ctx

	ratio, err := measureL4ThroughputRatio()
	if err != nil {
		t.Fatalf("throughput: %v", err)
	}
	t.Logf("L4 proxy/direct throughput ratio: %.3f (budget ≥ 0.6)", ratio)
	if ratio < 0.6 {
		t.Errorf("l4_throughput_ratio violated: %.3f < 0.6", ratio)
	}
}

// TestDNSLatency asserts 10k authoritative A queries answer with p99
// ≤ 1 ms (budget dns_query_p99_us).
func TestDNSLatency(t *testing.T) {
	skipNoPerf(t)
	p99 := measureDNSLatencyP99()
	t.Logf("authoritative DNS p99: %.0fµs over %d queries (budget ≤ 1000)", p99, dnsQueries)
	if p99 > 1000 {
		t.Errorf("dns_query_p99_us violated: %.0f > 1000", p99)
	}
}

func TestPairedMedianRatioIgnoresOutlierRounds(t *testing.T) {
	direct := []float64{100, 300, 100, 100, 100} // round 2's direct stream got a lucky core
	proxied := []float64{80, 80, 30, 80, 80}     // round 3's proxied stream hit a noisy neighbour
	if got := pairedMedianRatio(direct, proxied); got != 0.8 {
		t.Fatalf("pairedMedianRatio = %.3f, want 0.8", got)
	}
}
