# External Components

Every external tool and library doing heavy lifting in Expanse, what it does, and **what we would
otherwise have had to write**. This document exists because the project's original design drifted
into rewriting commodity infrastructure; keeping this inventory explicit and current is the
countermeasure.

**Rule:** nothing enters §5 (code we write) without first failing the test in §6.

---

## 1. Kernel and system components

These do the heavy lifting of the data plane. Expanse configures and orchestrates them.

| Component | Role in Expanse | What it saves us writing |
|---|---|---|
| **NixOS** | Base OS, declarative config, atomic generations, rollback | An entire OS build/packaging/upgrade system |
| **btrfs** | System filesystem; subvolume impermanence; checksums; self-healing RAID1 | A copy-on-write filesystem with integrity checking |
| **LVM2 / device-mapper** | Thin pools; volume create/resize/delete/snapshot; DRBD backing devices | A volume manager and thin-provisioning allocator |
| **DRBD 9** | Synchronous block replication, quorum, auto-promotion, bitmap resync | **`exvol`** — replication protocol, quorum ack, oplog, resync, failover and recovery (~9.7k lines, now scrapped) |
| **systemd** | Service supervision, cgroup limits, sandboxing, journald, sd_notify | A process supervisor, resource limiter and log collector |
| **WireGuard** (kernel) | Encrypted node-to-node overlay transport | A VPN protocol and its crypto |
| **nftables** (kernel) | Packet filtering, NAT, dynamic sets | A packet filter |
| **disko** | Declarative partitioning and filesystem creation at install | Imperative disk-layout scripting |
| **nix-daemon** | Builds and activates system closures | A package manager and deployment engine |

## 2. Go libraries

| Library | Role | What it saves us writing |
|---|---|---|
| `hashicorp/raft` | Consensus: leader election, log replication, snapshots | **A consensus algorithm.** We supply only an FSM (~400 lines) |
| `hashicorp/raft-boltdb`, `go.etcd.io/bbolt` | Raft log + stable store; single-node local store | An embedded transactional KV engine |
| `hashicorp/memberlist` | SWIM gossip; sub-second failure suspicion | A failure detector |
| `hashicorp/mdns` | `_expanse._tcp.local` discovery | An mDNS implementation |
| `miekg/dns` | DNS wire protocol, server and client | A DNS protocol stack |
| `google/nftables` | Programs nftables via netlink | An nftables expression encoder |
| `vishvananda/netlink` | Links, addresses, routes, ARP | Netlink plumbing |
| `wgctrl` | WireGuard device and peer configuration | WireGuard's netlink/genetlink interface |
| `coreos/go-systemd` | systemd D-Bus API, sd_notify | Parsing `systemctl` output (which we explicitly refuse to do) |
| `net/http/httputil` (stdlib) | L7 reverse proxying core | An HTTP proxy, including keep-alive, trailers and upgrade handling |
| `grpc` + `protobuf` | Internal node API, wire encoding, schema evolution | An RPC framework and serialization format |
| `spf13/cobra` | CLI structure, flags, help | Argument parsing and help generation |
| `santhosh-tekuri/jsonschema` | Block config validation (draft 2020-12) | A JSON Schema validator |
| `filippo.io/age` | Sealing secrets at rest | Envelope encryption |
| `robfig/cron` | Schedule expressions for scheduled blocks | A cron parser |
| `gopkg.in/yaml.v3` | Block manifests, installer config | A YAML parser |
| `google/uuid` | Identity generation | UUID generation |
| `anishathalye/porcupine` | Linearizability checking of recorded histories (test-only) | A Wing & Gong checker — our 246-line one was replaced after 430k generated histories and a 13.6k-op recorded run gave identical verdicts |

## 3. Planned — feature phases that will adopt rather than build

Named here so the adoption decision is made in advance, not rediscovered mid-phase.

| Feature (premise) | Component to adopt | Notes |
|---|---|---|
| SMB shares | **Samba**, clustered via ctdb or active/passive on DRBD | Approach chosen at phase task breakout |
| NFS exports | **nfs-kernel-server** (NFSv4.1) | Grace-period handling on failover is the hard part |
| iSCSI targets | **LIO / targetcli** | Active/passive over a DRBD-backed device via `SINGLETON`+VIP, not ALUA (`PHASE-04-TASKS.md` D1, revised — a DRBD Secondary node cannot host a real backstore at all); persistent reservation survival across failover is Stream C's own measurement, not assumed |
| Virtualized workloads | **QEMU/KVM**, via microvm.nix or cloud-hypervisor | Block-backed disks on DRBD volumes |
| Databases | **PostgreSQL**, HA via streaming replication | Application-level replication beats block-level here. Engine choice measured against MySQL/MariaDB and SQLite, not assumed: permissive license (no GPL/dual-commercial entanglement for a redistributed appliance, unlike MySQL); dominant in the self-hosted app ecosystem this project targets (28.6% of a 315-app survey vs. 15.6% MySQL/MariaDB, PostgreSQL+Redis the most common stack); lighter on this repo's own pinned channel (`postgresql-18.6` closure 150.8 MiB vs. `mariadb-server-11.4.12` closure 511.0 MiB, `nix path-info -Sh`, 2026-09-23); its HA-orchestration ecosystem (Patroni/repmgr/pg_auto_failover, all built on native streaming replication) is more mature and less fragmented than MySQL's equivalents, directly relevant to `PHASE-05-TASKS.md` D1; `pgvector` gives Phase 7's on-prem LLM work a first-party embedding-search story with no new component. SQLite ruled out on capability, not measurement — no server/network protocol, unsuited to a multi-node HA block by design. Orchestration layer: **decided — a native lease-gated controller**, not Patroni/repmgr/pg_auto_failover (`PHASE-05-TASKS.md` D1, `ARCHITECTURE.md` A28) — Patroni's own NixOS module dropped Raft as a supported DCS backend upstream, so adopting it means running etcd (or Consul/ZooKeeper/k8s) as a second consensus system unconditionally, not reusing the Raft lease this project already hardened in Phase 1; repmgr needs no external DCS but has no NixOS module either, so it needs the same integration work a native controller does; pg_auto_failover isn't packaged in nixpkgs at all. |
| On-prem LLMs | **Ollama** / llama.cpp, OpenAI-compatible surface | Already present as a block definition |
| Cloud backup | **restic** or **kopia** | Dedupe + encryption to S3-compatible storage; snapshot for consistency |
| Observability | **Prometheus-compatible** TSDB + Grafana | Export metrics; do not write a TSDB |

## 4. Removed

| Component | Why removed |
|---|---|
| **ZFS** | Out-of-tree kernel gate; untuned ARC competing with workloads on 4 GB targets. Keeping it alongside DRBD meant two out-of-tree modules gating every kernel upgrade, against a determinism pillar. Replaced by btrfs (system) + LVM (volume backing). |
| **NBD** | Was `exvol`'s device export path. DRBD presents `/dev/drbdN` directly; no userspace device server is needed. |
| **`exvol`** | Replaced wholesale by DRBD 9. See `DESIGN-AUDIT.md` for the measurements behind this. |

## 5. What we build, and why it is ours

Each of these is either the product itself or a thin policy layer that no external project provides
in the shape we need.

| Ours | Justification |
|---|---|
| **Block model** — schema, catalog, lifecycle, validation | The product. The Lego brick *is* Expanse. |
| **Scheduler** — filter/score placement, deterministic | Encodes the redundancy-by-default guarantee (refuse single replica at N≥3). No external scheduler enforces our policy without dragging in its own control plane. |
| **Cluster generations + rollback** | Mirrors NixOS generations at cluster scope; nothing external does this. |
| **Volume control plane** — placement, lease-gated promotion, CLI/UI surface | A thin layer *over* DRBD, not a replacement for it. LINSTOR does this job but carries a Java controller. |
| **Lease primitive** | ~500 lines over the Raft store. Has a defect history; hardened in Phase 1 rather than replaced, since adopting etcd's leases means adopting etcd. |
| **VIP allocation + holder** | Lease-arbitrated rather than VRRP, deliberately — VRRP has no quorum. keepalived would need our lease as a track script anyway. |
| **LB routing policy** | Store-derived routing over stdlib proxying. The protocol work is `httputil`'s; the policy is ours. |
| **DNS zone derivation** | Zone content from cluster state. The protocol is `miekg/dns`'s. |
| **Web UI, CLI, installer UX, `doctor`** | Product surface. |
| **Agent + reconcile loop** | Converges runtime-dynamic state (volume attach, VIP holder, block placement) that NixOS cannot express declaratively. |

## 6. The adoption test

Before writing a new subsystem, answer in writing:

1. **Which existing projects do this?** Name at least two.
2. **Why doesn't each fit?** Licence, footprint, dependency weight, or a capability gap — measured
   or explicitly marked unverified. "Footprint" requires a number, not an assertion.
3. **What is the estimated size of ours, and what did the last three estimates actually cost?**
   (Historical calibration: the DNS server was estimated at ~300 lines and shipped at 883.)
4. **What is the exit if ours fails?** If there is no fallback, that is itself a reason to adopt.

Record the answer in `ARCHITECTURE.md` §9. An unanswered adoption test is a blocked task.
