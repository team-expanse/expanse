# Networking

Expanse's network layer: a WireGuard mesh connecting every node, VIPs
for service endpoints, L4/L7 load balancing, cluster DNS, and a
default-deny nftables firewall. This document covers the address plan,
how each subsystem works, and how to debug it — the operational entry
point is `expanse doctor network`, which checks all of it live. For the
clustering machinery underneath (Raft, leases), see
[CLUSTERING.md](CLUSTERING.md).

## Address plan (spec §3)

```
      External clients
            │
            ▼  (VIP 192.168.1.100 — ARP announced by the lease holder)
    ┌───────────────────────────────────────────────┐
    │  Physical LAN  192.168.1.0/24                 │
    │   n1 .11        n2 .12        n3 .13          │
    └───────────────────────────────────────────────┘
        │  WireGuard overlay  10.42.0.0/16
        ▼  Service VIPs 10.43.0.0/16 (+ optional LAN VIPs)
```

| Range | Purpose |
|---|---|
| `10.42.0.0/16` | WireGuard overlay (`exp0`). Node N gets `10.42.N.0/24`; the node itself is `.1`. MTU 1420 (WireGuard overhead). |
| `10.43.0.0/16` | Internal service VIPs (cluster-only). |
| LAN subnet | External VIPs, allocated from a user-declared pool (e.g. `192.168.1.100-192.168.1.150`). The web UI's own VIP (`docs/WEB-UI.md`) takes one address from this pool at agent start, so size it one larger than the block VIPs you need. |
| `10.44.0.0/16` | Block-internal addresses (microvms/containers, Phase 09). |

Constants live in `internal/network/addrplan`; ports in
`internal/config/ports.go` (API 7443, Raft 7444, Join 7446, UI 8443 —
all over the overlay only, see the firewall section).

## Subsystems

### WireGuard mesh (`internal/network/mesh`)
Every node publishes a WireGuard public key under `/nodes/<id>` at
join time (CAS-published, so a rotation is a normal store write). Each
node's reconciler diffs the desired peer set (all other nodes' keys,
with the §3 overlay address as AllowedIPs) against the live `exp0`
device and applies the delta — a key rotation converges in one
reconcile pass, never a full interface rebuild. `expanse doctor
network` pings every peer and reports per-peer RTT.

### VIPs (`internal/network/vip`)
A VIP is exposed by whichever node holds its lease
(`vip:<addr>` in the Raft store, 10 s TTL, renewed every TTL/3). The
holder adds the address to the external interface and sends gratuitous
ARP (3 packets); losing the lease removes the address **first**, then
stops the listener — a replacement holder takes over within the lease
fence (measured ≤ 15 s from the external client, typically ~10 s).
Candidates are ordered by ready-replica count, ties by node ID. The
lease fence is the duplicate-holder safety argument: a new holder can
only take over after the old one's lease has expired, and a partitioned
holder stops serving when its own renewal fails.

### Load balancing (`internal/proxy`, wired in `internal/agent/lb.go`)
The VIP holder's listener slot is shared by the L4 and L7 balancers,
both driven by one pool table snapshotted from the store (`Pool`,
atomic swap — readers hold the old snapshot, in-flight requests never
see a torn table). L4 splices TCP connections round-robin across
healthy backends with drain-on-removal. A backend on the VIP holder's
own node is dialed from the VIP address, so a SINGLETON backend sees the
same peer address on whichever node serves it (NFS clients reclaim their
state by it); remote backends see the holder's node address. L7 is an `httputil.ReverseProxy`
routing by Host header (`<block>.<ns>.expanse.local`) and declared path
prefixes, injecting `X-Forwarded-For`/`X-Forwarded-Host`, retrying
idempotent methods on connection errors only (max 2). Backends are a
block's RUNNING placements, minus any whose readiness probe (run by
the replica's own node, `internal/agent/probes.go`) last failed.

### DNS (`internal/network/dns`, wired in `internal/agent/dns.go`)
An authoritative miekg/dns server on the node's overlay address
(`10.42.N.1:53`) serving the zone built from the store:
`<block>.<ns>.expanse.local` → VIP, `0.<block>…` → replica addresses,
SRV for declared ports. Zone snapshots swap atomically, so a scale-up
is visible on the next query (measured ≤ 5 s). Non-cluster names
forward to upstream resolvers with a short-lived positive/negative
cache.

### Firewall (`internal/network/firewall`)
One nftables table `inet expanse` per node: policy-drop `input` chain
with established/related, loopback, ICMP, mesh peers (dynamic set
`@cluster_peers`), cluster-service ports over `exp0` only, management
ports, and per-block VIP ports (dynamic sets `@vip_addresses`,
`@block_tcp_ports`, `@block_udp_ports`). After bootstrap the ruleset is
never reloaded — dynamic sets are updated with element add/delete only,
because a full reload drops conntrack (D5.6). Per-block network policy
(chain `blockpol`, priority 10) adds declared egress rules as intent.
Sync is internal (`internal/network/firewall.Sync`), driven
automatically by the agent's own reconcile loop, not a CLI verb — the
live ruleset is inspected read-only with `expanse firewall show`, and
`expanse firewall test <port>` checks whether a port is admitted by
the per-block sets.

## Troubleshooting: `expanse doctor network`

Run on any node (see `--help` for `--peer`, `--vip`, `--block` flags
that declare the targets to probe). Each check prints OK/FAIL/WARN/SKIP
with a hint; exit code 1 if anything failed.

| Check | What it does | A FAIL means | Fix |
|---|---|---|---|
| `interfaces` | External interface + addresses present | NIC down, renamed, no link | Check cabling/driver; `ip link` — the configured interface name must exist |
| `exp0` | `exp0` up, MTU 1420, `10.42.N.1` address | Mesh device misconfigured or missing | `expanse agent` not run/restarted? Check `/nodes/<id>` key in the store; MTU must stay 1420 |
| `overlay-peers` | Pings every declared peer over the overlay | A peer is down, or its WG key changed without reconvergence | `ping 1042.M.1`; check the peer's agent is running; a recent key rotation should converge in one reconcile pass — restart the agent if stuck |
| `df-mtu` | DF-ping at 1400-byte payload through the overlay | Path MTU broken (fragmentation blackhole) | A middlebox is stripping/dropping; keep MTU 1420 end to end |
| `port-matrix` | TCP-dials the cluster port matrix (7443/7444/7446) on every peer | Cluster port blocked over the overlay | Firewall on the peer or in between; the ports must be reachable over `exp0` (memberlist 7445 is not probed — it has no running listener) |
| `vips` | Exactly one holder per VIP | 0 holders = service down; ≥ 2 = split brain (also caught by `arp`) | 0: is any node's agent running with the block RUNNING? 2: fence bug — capture `ip addr` + lease state and file a bug |
| `arp` | VIP resolves to ONE MAC from every vantage point | Different MACs from different nodes = duplicate holders | Same as vips ≥ 2; the chaos suite (`RUN_CHAOS=1 go test ./test/chaos/...`) covers this continuously |
| `block-dns` | `<block>.<ns>.expanse.local` resolves | Zone builder stale or DNS listener down | Check the block exists and is RUNNING; listener binds `10.42.N.1:53` (see n3's `dns listener failed` if the overlay is not up yet) |
| `upstream-dns` | A public name forwards | No upstream reachability | Not fatal (authoritative names still work); check resolv.conf/upstream reachability |
| `firewall` | `expanse` nftables table loaded, counters live | Ruleset missing, or read-back errors | Restart the agent to re-bootstrap the table (sync itself is internal, driven by the reconcile loop, not a CLI verb); never full-reload manually (drops conntrack, D5.6) |
| `conntrack` | Table usage + per-block connection counts | ≥ 90% full (FAIL) / ≥ 70% (WARN) | Raise `nf_conntrack_max`; a leaking block? `conntrack -L` to find it |
| `time-sync` | Chrony offset < 100 ms | Clock drift breaks lease expiry judgments | `chronyc sources`; NTP must be working — the lease safety argument tolerates bounded skew only |

## M3 demo

`nix build .#checks.x86_64-linux.m3-demo -L` runs the scripted demo
against three VMs and an external client: deploy nginx (3 replicas,
VIP), curl the VIP from outside, hard power off the VIP holder, watch
curl recover (typically ~10 s), power the node back on and show the VIP
stays put. The ≤ 15 s budget (G5.4) is enforced by the stricter
`net-vip-failover` check; the demo prints its measured recovery time.

## Performance

Budgets live in `test/perf/budgets.yaml`; run with
`RUN_PERF=1 go test ./test/perf/...`. Measured on loopback: L7 ≥ 20k
req/s, added p99 latency ≤ 1 ms vs a direct-to-backend baseline, L4 ≥
80% of direct-loopback throughput, authoritative DNS p99 ≤ 1 ms.
`proxy_rss_bytes` (≤ 30 MB under 100 concurrent connections) and
`vip_failover_ms` (≤ 5000 for the announcement to follow the lease) are
measured in the VM tests, where the isolated agent process and real
lease churn exist.
