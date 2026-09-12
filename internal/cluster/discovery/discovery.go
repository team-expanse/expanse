// Package discovery implements cluster discovery (§4.6): mDNS
// advertisement of `_expanse._tcp.local` with TXT records
// {cluster_id, node_id, role, api_port}, a browse helper, and static
// seed parsing.
//
// Discovery never grants membership. It only surfaces candidate
// addresses for the installer and `cluster join --discover`; joining
// still requires a valid token (§4.5).
package discovery

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/mdns"

	"github.com/expanse/expanse/internal/errors"
)

// ServiceName is the mDNS service type advertised by every node.
const ServiceName = "_expanse._tcp"

// RoleUnjoined is advertised by nodes that are installed but not yet
// part of a cluster (cluster_id empty).
const RoleUnjoined = "unjoined"

// Record is one discovered node.
type Record struct {
	ClusterID string // "" for unjoined nodes
	NodeID    string
	Role      string
	APIPort   int
	Addr      string // advertised IP
}

// Advertiser publishes this node on the local network via mDNS until
// Close is called.
type Advertiser struct {
	srv *mdns.Server
}

// Advertise starts answering mDNS queries for this node. The instance
// name is the node ID so re-advertisements (role changes, restarts)
// update in place.
func Advertise(clusterID, nodeID, role string, apiPort int) (*Advertiser, error) {
	txt := []string{
		"cluster_id=" + clusterID,
		"node_id=" + nodeID,
		"role=" + role,
		"api_port=" + strconv.Itoa(apiPort),
	}
	svc := &mdns.MDNSService{
		Instance: nodeID,
		Service:  ServiceName + ".",
		Domain:   "local",
		HostName: nodeID + ".expanse.local.",
		Port:     apiPort,
		TXT:      txt,
	}
	srv, err := mdns.NewServer(&mdns.Config{Zone: svc})
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "discovery.Advertise", "start mDNS responder")
	}
	return &Advertiser{srv: srv}, nil
}

// Close stops answering queries.
func (a *Advertiser) Close() error {
	return a.srv.Shutdown()
}

// Browse collects records for the given timeout (bounded also by ctx).
// It never returns an error for an empty network — an empty slice means
// nothing was found.
func Browse(ctx context.Context, timeout time.Duration) ([]Record, error) {
	entries := make(chan *mdns.ServiceEntry, 64)
	qp := &mdns.QueryParam{
		Service: ServiceName,
		Domain:  "local",
		Timeout: timeout,
		Entries: entries,
	}
	var records []Record
	done := make(chan struct{})
	go func() {
		defer close(done)
		// QueryContext only errors when the transport fails; an empty
		// network is not an error. Transport failures surface as an
		// empty result — acceptable for a best-effort discovery scan.
		_ = mdns.QueryContext(ctx, qp)
	}()

	// Collect until the query's own timeout fires.
	collectDone := time.After(timeout + 500*time.Millisecond)
	for {
		select {
		case e := <-entries:
			if e == nil {
				continue
			}
			rec := recordFromEntry(e)
			if rec != nil {
				records = append(records, *rec)
			}
		case <-collectDone:
			return records, nil
		case <-ctx.Done():
			return records, nil
		case <-done:
			// QueryContext finished early; drain any remaining entries.
			for {
				select {
				case e := <-entries:
					if e == nil {
						continue
					}
					if rec := recordFromEntry(e); rec != nil {
						records = append(records, *rec)
					}
				default:
					return records, nil
				}
			}
		}
	}
}

func recordFromEntry(e *mdns.ServiceEntry) *Record {
	txt := parseTXT(e.InfoFields)
	rec := &Record{
		NodeID:  txt["node_id"],
		Role:    txt["role"],
		APIPort: e.Port,
	}
	if e.AddrV4 != nil {
		rec.Addr = e.AddrV4.String()
	} else if e.AddrV6IPAddr != nil {
		rec.Addr = e.AddrV6IPAddr.String()
	} else if e.Addr != nil {
		rec.Addr = e.Addr.String()
	}
	if rec.NodeID == "" {
		// Fall back to the instance name.
		rec.NodeID = strings.TrimSuffix(e.Name, "."+ServiceName+".local.")
	}
	rec.ClusterID = txt["cluster_id"]
	return rec
}

// parseTXT splits "k=v" TXT strings into a map. The mdns library
// delivers TXT as a raw string slice.
func parseTXT(fields []string) map[string]string {
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		if k, v, ok := strings.Cut(f, "="); ok {
			out[k] = v
		}
	}
	return out
}

// ParseSeeds parses a comma-separated list of static seed addresses
// (`--seeds ip:7444,ip:7444`). Returns host:port strings. Errors name
// the offending entry so the operator can fix the flag.
func ParseSeeds(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		host, port, err := net.SplitHostPort(part)
		if err != nil {
			return nil, errors.New(errors.KindInvalid, "discovery.ParseSeeds",
				fmt.Sprintf("seed %q is not host:port", part))
		}
		if host == "" {
			return nil, errors.New(errors.KindInvalid, "discovery.ParseSeeds",
				fmt.Sprintf("seed %q has empty host", part))
		}
		if _, err := strconv.Atoi(port); err != nil {
			return nil, errors.New(errors.KindInvalid, "discovery.ParseSeeds",
				fmt.Sprintf("seed %q has non-numeric port", part))
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, errors.New(errors.KindInvalid, "discovery.ParseSeeds", "no seeds given")
	}
	return out, nil
}
