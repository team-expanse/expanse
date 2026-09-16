package dns

import (
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func query(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

func aRecord(name string, qtype uint16) *dns.A {
	return &dns.A{Hdr: dns.RR_Header{Name: dns.Fqdn(name), Rrtype: qtype, Class: dns.ClassINET, Ttl: 5}, A: netip.MustParseAddr("192.168.1.100").AsSlice()}
}

func TestAuthoritativeZoneServed(t *testing.T) {
	s := NewServer()
	s.SetZone([]Record{
		{Name: "web.default.expanse.local.", Type: "A", TTL: 5, Data: "192.168.1.100"},
		{Name: "web.default.expanse.local.", Type: "A", TTL: 5, Data: "192.168.1.101"},
		{Name: "0.web.default.expanse.local.", Type: "A", TTL: 5, Data: "10.42.1.1"},
		{Name: "_http._tcp.web.default.expanse.local.", Type: "SRV", TTL: 5, Data: "0 0 80 0.web.default.expanse.local."},
	})

	// A record, multiple answers.
	resp := s.Answer(query("web.default.expanse.local", dns.TypeA))
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 2 || !resp.Authoritative {
		t.Fatalf("A lookup: rcode=%d n=%d aa=%v", resp.Rcode, len(resp.Answer), resp.Authoritative)
	}

	// SRV.
	resp = s.Answer(query("_http._tcp.web.default.expanse.local", dns.TypeSRV))
	if len(resp.Answer) != 1 {
		t.Fatalf("SRV lookup: %d answers", len(resp.Answer))
	}
	srv, ok := resp.Answer[0].(*dns.SRV)
	if !ok || srv.Port != 80 || srv.Target != "0.web.default.expanse.local." {
		t.Fatalf("SRV content: %+v", srv)
	}

	// NODATA: known name, wrong type.
	resp = s.Answer(query("web.default.expanse.local", dns.TypeAAAA))
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 0 || !resp.Authoritative {
		t.Fatalf("NODATA: rcode=%d n=%d", resp.Rcode, len(resp.Answer))
	}

	// NXDOMAIN: unknown name inside the zone.
	resp = s.Answer(query("nope.default.expanse.local", dns.TypeA))
	if resp.Rcode != dns.RcodeNameError || !resp.Authoritative {
		t.Fatalf("NXDOMAIN: rcode=%d aa=%v", resp.Rcode, resp.Authoritative)
	}
}

func TestForwardingAndCache(t *testing.T) {
	upstream := dns.NewServeMux()
	upstream.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		name := r.Question[0].Name
		if strings.HasPrefix(name, "missing.") {
			m.Rcode = dns.RcodeNameError // negative answer, no SOA
		} else if r.Question[0].Qtype == dns.TypeA {
			m.Answer = append(m.Answer, aRecord(name, dns.TypeA))
			m.Answer[0].(*dns.A).Hdr.Ttl = 300 // clamped below
		}
		w.WriteMsg(m)
	})
	upReady := make(chan string, 1)
	go func() {
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return
		}
		defer conn.Close()
		upReady <- conn.LocalAddr().String()
		(&dns.Server{PacketConn: conn, Handler: upstream}).ActivateAndServe()
	}()
	var addr string
	select {
	case addr = <-upReady:
	case <-time.After(5 * time.Second):
		t.Fatal("no upstream listener")
	}

	s := NewServer()
	s.Upstreams = []string{addr}
	q := query("example.com", dns.TypeA)
	q.Id = 42

	resp := s.Answer(q) // MISS
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("forward miss: %+v", resp)
	}
	if resp.Id != 42 {
		t.Fatalf("response Id not echoed: %d", resp.Id)
	}
	if resp.Answer[0].Header().Ttl != 60 {
		t.Fatalf("positive TTL not clamped to %v: %d", MaxPositiveTTL, resp.Answer[0].Header().Ttl)
	}

	// HIT: kill the upstream; the cached answer must survive.
	s.Upstreams = nil
	resp2 := s.Answer(q)
	if resp2.Rcode != dns.RcodeSuccess || len(resp2.Answer) != 1 {
		t.Fatalf("forward hit after upstream death: %+v", resp2)
	}
	if resp2.Id != 42 {
		t.Fatalf("cached response Id: %d", resp2.Id)
	}

	// Negative cache: upstream NXDOMAIN is cached like a hit.
	s.Upstreams = []string{addr}
	qx := query("missing.example.com", dns.TypeA)
	r1 := s.Answer(qx)
	if r1.Rcode != dns.RcodeNameError {
		t.Fatalf("negative miss: %d", r1.Rcode)
	}
	r2 := s.Answer(qx)
	if r2.Rcode != dns.RcodeNameError {
		t.Fatalf("negative hit: %d", r2.Rcode)
	}
}

func TestNoUpstreamServFail(t *testing.T) {
	s := NewServer()
	resp := s.Answer(query("example.com", dns.TypeA))
	if resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("expected SERVFAIL, got %d", resp.Rcode)
	}
}

func TestMDNSPlans(t *testing.T) {
	n1 := node("n1", "10.42.1.1")
	in := Input{
		Nodes: []NodeInfo{*n1},
		Blocks: []BlockInfo{
			{
				Namespace: "default", Name: "Web", MDNS: true,
				VIP:      netip.MustParsePrefix("192.168.1.100/24"),
				Replicas: []Replica{{Index: 0, Node: n1, Ready: true}},
			},
			{Namespace: "default", Name: "nolabel", // opt-out: no plan
				Replicas: []Replica{{Index: 0, Node: n1, Ready: true}}},
			{Namespace: "default", Name: "down", MDNS: true}, // no replicas: nothing
		},
	}
	got := MDNSPlans(in)
	if len(got) != 1 || got[0].Name != "web.default.local." || len(got[0].IPs) != 1 ||
		!got[0].IPs[0].Equal(netip.MustParseAddr("192.168.1.100").AsSlice()) {
		t.Fatalf("plans: %+v", got)
	}
}
