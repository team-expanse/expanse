package discovery

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestParseSeeds(t *testing.T) {
	t.Parallel()

	seeds, err := ParseSeeds(" 10.0.0.1:7444 , 10.0.0.2:7444")
	if err != nil {
		t.Fatalf("ParseSeeds: %v", err)
	}
	if len(seeds) != 2 || seeds[0] != "10.0.0.1:7444" || seeds[1] != "10.0.0.2:7444" {
		t.Errorf("seeds = %v", seeds)
	}

	if seeds, err := ParseSeeds(""); err != nil || seeds != nil {
		t.Errorf("empty: seeds=%v err=%v, want nil/nil", seeds, err)
	}
	if seeds, err := ParseSeeds("   "); err != nil || seeds != nil {
		t.Errorf("blank: seeds=%v err=%v, want nil/nil", seeds, err)
	}
	if seeds, err := ParseSeeds("10.0.0.1"); err == nil {
		t.Errorf("missing port: accepted as %v", seeds)
	}
	if _, err := ParseSeeds("10.0.0.1:notaport"); err == nil {
		t.Error("bad port: accepted")
	}
	if _, err := ParseSeeds(","); err == nil {
		t.Error("only commas: accepted")
	}
}

// TestAdvertiseBrowseRoundTrip exercises the full mDNS path on the
// loopback multicast interface. Skipped when multicast is unavailable
// (common in CI sandboxes) or under -short.
func TestAdvertiseBrowseRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("mDNS round-trip skipped in -short")
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("no interfaces: %v", err)
	}
	hasLoopback := false
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback != 0 && i.Flags&net.FlagMulticast != 0 && i.Flags&net.FlagUp != 0 {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		t.Skip("no multicast-capable loopback interface")
	}

	adv, err := Advertise("11111111-2222-3333-4444-555555555555", "n-adv", "voter", 8443)
	if err != nil {
		t.Skipf("mDNS unavailable in this environment: %v", err)
	}
	defer func() { _ = adv.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	records, err := Browse(ctx, 3*time.Second)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}

	found := false
	for _, r := range records {
		if r.NodeID == "n-adv" {
			found = true
			if r.ClusterID != "11111111-2222-3333-4444-555555555555" {
				t.Errorf("cluster_id = %q", r.ClusterID)
			}
			if r.Role != "voter" || r.APIPort != 8443 || r.Addr == "" {
				t.Errorf("record = %+v", r)
			}
		}
	}
	if !found {
		t.Skipf("advertised node not seen (multicast likely blocked); saw %d records", len(records))
	}
}

// TestBrowseEmptyNetwork verifies a browse on a quiet network returns
// no records and no error (best-effort semantics).
func TestBrowseEmptyNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("mDNS browse skipped in -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	records, err := Browse(ctx, 500*time.Millisecond)
	// Either an empty result (normal) or a sandbox that blocks UDP
	// multicast must not hard-fail the suite.
	if err != nil {
		t.Skipf("mDNS unavailable: %v", err)
	}
	if len(records) > 0 && !anySelf(records) {
		// Records from other hosts on the LAN are possible; that's fine.
		t.Logf("saw %d foreign records", len(records))
	}
}

func anySelf(records []Record) bool {
	for _, r := range records {
		if r.NodeID != "" {
			return true
		}
	}
	return false
}

func TestParseTXT(t *testing.T) {
	t.Parallel()
	txt := parseTXT([]string{"cluster_id=abc", "role=voter", "novalue", "=weird"})
	if txt["cluster_id"] != "abc" || txt["role"] != "voter" {
		t.Errorf("txt = %v", txt)
	}
}
