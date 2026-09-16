// server.go is the DNS server (T17, PHASE05.md §4.4): authoritative
// A/SRV answers for expanse.local. served from the T16 zone builder's
// current snapshot (swapped atomically on every rebuild), everything
// else forwarded to upstream resolvers with a small positive/negative
// cache. The server holds no store state of its own — the zone source
// (source.go) drives SetZone from watch events, targeting ≤5 s
// propagation.
package dns

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// forwardTimeout bounds a single upstream exchange.
const forwardTimeout = 2 * time.Second

// cache defaults. Positive entries honor the upstream's TTL, clamped
// to keep a stuck answer from living forever; negative answers have no
// upstream TTL (no SOA minimum to trust) so they get a short fixed
// lifetime — long enough to absorb a retry storm, short enough that a
// freshly-created public name resolves on the next probe.
const (
	MaxPositiveTTL = 60 * time.Second
	NegativeTTL    = 30 * time.Second
)

// zoneSnapshot is an immutable view of the authoritative record set.
type zoneSnapshot struct {
	byName map[string][]Record // lowercased FQDN → records
}

// Server is the DNS handler. Zone is the current authoritative record
// set (swapped atomically by the zone source); Upstreams are the
// "ip:port" resolvers non-cluster queries are forwarded to, tried in
// order.
type Server struct {
	Upstreams []string

	mu    sync.Mutex
	zone  atomicZone
	cache map[cacheKey]cacheEntry
}

// atomicZone wraps the zone pointer swap.
type atomicZone struct {
	mu sync.RWMutex
	s  *zoneSnapshot
}

func (a *atomicZone) load() *zoneSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.s
}

func (a *atomicZone) store(s *zoneSnapshot) {
	a.mu.Lock()
	a.s = s
	a.mu.Unlock()
}

type cacheKey struct {
	name  string
	qtype uint16
}

type cacheEntry struct {
	msg    *dns.Msg // Id always 0; cloned per hit
	expire time.Time
}

// NewServer returns a server. SetZone must be called before it can
// answer authoritative queries; with no zone it still forwards.
func NewServer() *Server {
	return &Server{cache: map[cacheKey]cacheEntry{}}
}

// SetZone atomically replaces the authoritative record set.
func (s *Server) SetZone(recs []Record) {
	snap := &zoneSnapshot{byName: map[string][]Record{}}
	for _, r := range recs {
		snap.byName[strings.ToLower(r.Name)] = append(snap.byName[strings.ToLower(r.Name)], r)
	}
	s.zone.store(snap)
}

// ServeDNS answers one query: authoritative for expanse.local.,
// forwarded (and cached) for everything else.
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	resp := s.Answer(r)
	if err := w.WriteMsg(resp); err != nil {
		// Write failures (client went away) are not server errors.
		_ = err
	}
}

// Answer is ServeDNS without the socket: it builds the response for a
// request message and is what the unit tests drive.
func (s *Server) Answer(r *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	if len(r.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
		return m
	}
	q := r.Question[0]
	name := strings.ToLower(q.Name)

	if strings.HasSuffix(name, "."+Zone) || name == Zone {
		return s.answerAuthoritative(m, name, q.Qtype)
	}
	return s.answerForwarded(m, r, q)
}

// answerAuthoritative serves the built zone: matching records as the
// answer section, AA set; a known name without the queried type is
// NODATA (NOERROR, empty); an unknown name is NXDOMAIN.
func (s *Server) answerAuthoritative(m *dns.Msg, name string, qtype uint16) *dns.Msg {
	snap := s.zone.load()
	m.Authoritative = true
	if snap == nil {
		m.Rcode = dns.RcodeNameError
		return m
	}
	recs := snap.byName[name]
	if len(recs) == 0 {
		m.Rcode = dns.RcodeNameError
		return m
	}
	for _, rec := range recs {
		switch {
		case rec.Type == "A" && qtype == dns.TypeA:
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: rec.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: rec.TTL},
				A:   net.ParseIP(rec.Data),
			})
		case rec.Type == "SRV" && qtype == dns.TypeSRV:
			m.Answer = append(m.Answer, srvRR(rec))
		}
	}
	return m // found name, wrong type → NODATA
}

// answerForwarded resolves outside-cluster names via the configured
// upstreams, first hit wins, result cached (positive and negative).
func (s *Server) answerForwarded(m *dns.Msg, r *dns.Msg, q dns.Question) *dns.Msg {
	key := cacheKey{name: strings.ToLower(q.Name), qtype: q.Qtype}
	s.mu.Lock()
	hit, ok := s.cache[key]
	if ok && time.Now().After(hit.expire) {
		delete(s.cache, key)
		ok = false
	}
	s.mu.Unlock()
	if ok {
		ans := hit.msg.Copy()
		ans.Id = r.Id
		return ans
	}

	var last *dns.Msg
	for _, up := range s.Upstreams {
		c := &dns.Client{Timeout: forwardTimeout}
		resp, _, err := c.Exchange(r, up)
		if err != nil || resp == nil {
			continue
		}
		last = resp
		break
	}
	if last == nil {
		m.Rcode = dns.RcodeServerFailure
		return m
	}

	// Cache: keep the full reply (Id zeroed) with a clamped TTL.
	ttl := MaxPositiveTTL
	if last.Rcode != dns.RcodeSuccess || len(last.Answer) == 0 {
		ttl = NegativeTTL
	} else {
		ttl = 0
		for _, rr := range last.Answer {
			if t := time.Duration(rr.Header().Ttl) * time.Second; ttl == 0 || t < ttl {
				ttl = t
			}
		}
		if ttl <= 0 || ttl > MaxPositiveTTL {
			ttl = MaxPositiveTTL
		}
	}
	// The served answer carries the clamped TTL too, so downstream
	// stub resolvers don't out-cache us.
	for _, rr := range last.Answer {
		rr.Header().Ttl = uint32(ttl.Seconds())
	}
	cached := last.Copy()
	cached.Id = 0
	s.mu.Lock()
	s.cache[key] = cacheEntry{msg: cached, expire: time.Now().Add(ttl)}
	s.mu.Unlock()

	last.Id = r.Id
	return last
}

func srvRR(rec Record) dns.RR {
	// Data: "priority weight port target".
	f := strings.Fields(rec.Data)
	if len(f) != 4 {
		return nil
	}
	var prio, weight, port uint16
	for i := range 3 {
		n := 0
		for _, c := range f[i] {
			n = n*10 + int(c-'0')
		}
		switch i {
		case 0:
			prio = uint16(n)
		case 1:
			weight = uint16(n)
		case 2:
			port = uint16(n)
		}
	}
	return &dns.SRV{
		Hdr:      dns.RR_Header{Name: rec.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: rec.TTL},
		Priority: prio,
		Weight:   weight,
		Port:     port,
		Target:   f[3],
	}
}
