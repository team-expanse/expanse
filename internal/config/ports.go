package config

// Fixed port assignments for all node↔node traffic. These appear in
// firewall rules forever — do not change them casually (spec Phase 03 §3).
const (
	PortAPI        = 7443 // gRPC API (mTLS)
	PortRaft       = 7444 // Raft transport (mTLS)
	PortMemberlist = 7445 // memberlist gossip (TCP+UDP)
	PortJoin       = 7446 // Join/bootstrap service (TLS, token-authenticated)
	PortMDNS       = 5353 // mDNS discovery (UDP)
	PortUI         = 8443 // Web UI (Phase 08)
	PortExvol      = 9440 // exvol replication transport (Phase 06 §4.3)
)
