# Expanse — Product Design Document

**Version:** 1.0 (Design Baseline)
**Status:** Draft for implementation
**Source of truth for scope:** [`PREMISE.md`](./PREMISE.md)

---

## 1. Executive Summary

Expanse is a **NixOS-based, clustered server operating environment** with a Go + HTMX control plane. It targets homelab operators and small-to-medium businesses who want the reliability characteristics of hyperscaler infrastructure (replication, load balancing, HA, no SPOF) without the operational complexity of assembling Kubernetes + Ceph + Terraform + Prometheus + a dozen other tools by hand.

The core thesis: **declarative infrastructure should be composable like Lego bricks.** Every capability — storage export, database, VM, LLM endpoint, web service — is a *block*. Blocks snap into a cluster. The cluster guarantees that every block is replicated, balanced, and highly available by default, not by configuration.

### 1.1 Design Pillars

| Pillar | Meaning | Enforcement Mechanism |
|---|---|---|
| **Deterministic** | Same inputs → bit-identical system state | NixOS flakes, pinned inputs, content-addressed store |
| **Flexible** | Compose arbitrary workloads | Block abstraction over systemd/microvm/OCI |
| **Extendable** | Third parties add blocks without forking | Block SDK + registry + stable ABI |
| **Expansive** | Scale from 1 laptop to N racks | Same code path at all sizes; HA engages at N≥3 |
| **Redundant by default** | HA is not opt-in | Scheduler refuses to place a single replica when N≥3 |
| **Lightweight** | Control plane < 150 MB RSS, < 3% CPU idle | Single static Go binary, no JVM/Python/Node runtime |

### 1.2 Explicit Non-Goals

- Not a Kubernetes distribution. We do not implement the Kubernetes API.
- Not a general-purpose cloud (no multi-tenancy with hostile tenants in v1).
- Not a desktop OS.
- No JavaScript SPA. HTMX + server-rendered templates only.
- No agent-per-container sidecars. One node agent per machine.

---

## 2. Target Users & Scenarios

### 2.1 Personas

**P1 — Homelab Hank.** 3 old ThinkPads and a NAS drive. Wants Plex, Home Assistant, a Postgres for side projects, and a local LLM. Currently runs Proxmox + Docker Compose and dreads updates.
*Success:* Boots installer on 3 laptops, they auto-discover each other, he clicks "Deploy Postgres," and it survives him unplugging one laptop.

**P2 — SMB Sysadmin Sam.** 40-person company, 4 rack servers, needs SMB file shares, a couple of LOB app VMs, nightly offsite backups, and to pass a basic security audit.
*Success:* Declares the whole company infra in one `expanse.nix`, commits it to git, and can rebuild the entire org from that file plus a backup bucket.

**P3 — Platform Builder Pat.** Wants to ship a product on top of Expanse. Needs a stable extension API.
*Success:* Writes a block definition in Nix + Go, publishes to a flake, customers `expanse block install github:pat/cool-block`.

### 2.2 Scenario Walkthroughs (drive acceptance tests)

1. **Bare metal to running cluster in 20 minutes.** Boot ISO on 3 machines → they form a cluster → web UI reachable at a VIP → first block deployed.
2. **Kill a node mid-write.** `kill -9` the leader during a database write. Write either completes or cleanly fails; no data corruption; new leader within 10 s.
3. **Rollback a bad config.** Apply a config that breaks DNS. UI shows unhealthy. One click reverts to previous generation cluster-wide.
4. **Disaster recovery.** Cluster physically destroyed. New hardware + backup credentials + one command → full restore of configs *and* data.
5. **Grow the cluster.** Plug in a 4th machine. It joins, storage rebalances, no manual intervention.

---

## 3. System Architecture

### 3.1 Layer Model

```
┌───────────────────────────────────────────────────────────────┐
│  L6  Web UI (Go html/template + HTMX + Tailwind, SSE)        │
├───────────────────────────────────────────────────────────────┤
│  L5  API  (gRPC internal, REST+HTMX external, expansectl CLI)│
├───────────────────────────────────────────────────────────────┤
│  L4  Blocks (services, DBs, VMs, LLMs, shares) + Block SDK   │
├───────────────────────────────────────────────────────────────┤
│  L3  Orchestration (scheduler, placement, health, failover)   │
├───────────────────────────────────────────────────────────────┤
│  L2  Cluster Substrate (Raft state store, membership, gossip, │
│      certificate authority, secret store, VIP/LB, DNS)        │
├───────────────────────────────────────────────────────────────┤
│  L1  Node Runtime (expanse-agent, nix-daemon driver,          │
│      storage engine, network engine, metrics)                 │
├───────────────────────────────────────────────────────────────┤
│  L0  NixOS Base Image (flake, modules, hardened kernel)       │
└───────────────────────────────────────────────────────────────┘
```

### 3.2 Processes on Every Node

| Process | Language | Role |
|---|---|---|
| `expansed` | Go | Node agent. Owns local state, runs Raft member, executes reconcile loop, exposes gRPC on :7443 (mTLS). |
| `expanse-ui` | Go | Web server. Runs on every node; VIP routes to a healthy one. Can be same binary, different mode. |
| `nix-daemon` | C++ | Stock NixOS. Driven by `expansed` via `nix build`/`switch-to-configuration`. |
| `expanse-proxy` | Go | L4/L7 load balancer + VIP owner (embedded, not HAProxy). |
| Storage daemons | varies | Ceph OSD/MON *or* built-in replicated volume manager (see §5). |

**One binary policy:** `expansed`, `expanse-ui`, `expanse-proxy`, and `expansectl` are all subcommands of a single static binary `expanse` to minimize footprint and version skew.

### 3.3 Control Plane State Model

All cluster state lives in a **Raft-replicated, versioned key-value store** embedded in `expansed` (hashicorp/raft + BoltDB log store). No external etcd.

```
/cluster/meta                 → cluster ID, version, created
/nodes/<node-id>              → NodeSpec + NodeStatus (heartbeat, capacity)
/blocks/<block-id>/spec       → desired state (user intent)
/blocks/<block-id>/status     → observed state (agent-reported)
/placements/<block-id>        → list of (node-id, replica-index, role)
/volumes/<vol-id>             → size, replication factor, placement, health
/networks/<net-id>            → VIPs, subnets, DNS records
/secrets/<path>               → sealed blobs (age-encrypted, key in TPM/keyfile)
/generations/<n>              → immutable snapshot of full desired state
/events/                      → append-only audit + event log (ring buffered)
```

**Generations.** Every accepted change creates generation N+1 as a full immutable snapshot. Rollback = "make generation N-1 current." This mirrors NixOS generations at the cluster level and is the mechanism behind Scenario 3.

### 3.4 Reconciliation Loop (the heart of the system)

```
     desired (Raft)                observed (local probes)
           │                                │
           └────────► diff engine ◄─────────┘
                          │
                   ordered plan (DAG)
                          │
              ┌───────────┴───────────┐
              ▼                       ▼
        nix realize             runtime actions
     (build + switch)       (start/stop/migrate/
                             attach volume/update LB)
                          │
                   verify + report status
                          │
                  ┌───────┴────────┐
                  ▼                ▼
             converged        failed → backoff,
                              emit event, mark degraded
```

Properties:
- **Idempotent.** Running the loop twice changes nothing the second time.
- **Level-triggered,** not edge-triggered. Missed events self-heal.
- **Bounded.** Each tick has a deadline (default 5 min); overruns are aborted and retried.
- **Serialized per resource,** parallel across independent resources.

---

## 4. Networking Design

### 4.1 Cluster Fabric

- **Discovery:** mDNS (`_expanse._tcp.local`) on the LAN for zero-config, plus static seed list for routed networks.
- **Membership/gossip:** hashicorp/memberlist for fast failure detection (sub-second suspicion, ~3 s confirm). Raft handles consistency; memberlist handles liveness.
- **Encryption:** All node↔node traffic is mTLS. Cluster-internal CA bootstrapped at init; certs auto-rotate every 30 days with 90-day CA.
- **Overlay:** WireGuard mesh (`wg0`, 10.42.0.0/16) so blocks get stable IPs independent of physical topology. Each node gets a /24.

### 4.2 Service Exposure

Every block that serves traffic gets:
1. A **stable VIP** from the service range (default 10.43.0.0/16), announced by whichever node currently holds it.
2. **VIP failover** via unsolicited ARP/NDP from the new holder, arbitrated by Raft lease (not VRRP — avoids split-brain via quorum).
3. **L4 or L7 load balancing** in `expanse-proxy` across healthy replicas, with health-check-driven backend pools.
4. A **DNS name** `<block>.<cluster>.expanse.local` served by the built-in authoritative resolver on every node.

### 4.3 Health Checking Contract

Three independent signals, all required for "healthy":
- **Liveness:** process/VM exists and hasn't crash-looped (≥3 restarts in 60 s = unhealthy).
- **Readiness:** block-declared probe (TCP connect, HTTP status, or exec script) passes.
- **Data:** replication lag within block-declared bound; quorum present for stateful blocks.

---

## 5. Storage Design

### 5.1 Two-Tier Strategy

Expanse deliberately ships **two** storage engines because one size does not fit the 3-laptop-to-rack range.

**Tier A — `exvol` (built-in, default, N=1..4 nodes).**
DRBD-style synchronous block replication written in Go over the WireGuard mesh, layered on ZFS zvols.
- Replication factor 2 or 3, synchronous, quorum writes.
- Automatic resync on rejoin using ZFS snapshot diffs (fast, not full-copy).
- Primary/secondary with Raft-arbitrated promotion.
- Why: Ceph needs ~3 GB RAM per OSD and misbehaves below 3 nodes. `exvol` runs in ~200 MB.

**Tier B — Ceph (opt-in, N≥5 nodes, ≥8 GB RAM/node).**
Standard RADOS, managed by Expanse as a block. Gives erasure coding, CephFS, RGW/S3.
- Expanse generates all Ceph config, manages MON quorum placement and OSD lifecycle.
- Migration path: `expansectl volume migrate --to ceph` does online copy + cutover.

### 5.2 Filesystem Layout per Node

```
/nix/store                  read-only, content-addressed  (NixOS)
/persist                    ZFS dataset, snapshotted, the ONLY writable state
  /persist/expanse/raft     Raft log + snapshots
  /persist/expanse/secrets  sealed secrets
  /persist/blocks/<id>      per-block persistent data
  /persist/volumes          zvols for exvol
/var, /etc, /home           tmpfs or bind-mounted from /persist (impermanence)
```

**Impermanence is mandatory.** The root filesystem is wiped on every boot. This makes "it works on a fresh install" and "it works on my 2-year-old node" the same statement — a direct enforcement of the determinism pillar.

### 5.3 Network Storage Exports (per PREMISE)

| Protocol | Implementation | HA Mechanism |
|---|---|---|
| **SMB** | Samba, `ctdb` for clustered state | VIP + ctdb recovery lock on exvol/Ceph |
| **NFS** | nfs-kernel-server, NFSv4.1 | VIP + shared state dir on replicated volume; grace period handling |
| **iSCSI** | LIO/targetcli | VIP + ALUA multipath, PR (persistent reservations) on replicated backing store |

All three are modeled as blocks that *consume* a volume and *produce* a VIP endpoint.

---

## 6. The Block Model (core abstraction)

### 6.1 Definition

A **Block** is a declarative unit of workload. It is the Lego brick.

```nix
{
  block = {
    name = "postgres-main";
    type = "database/postgresql";
    version = "16.4";

    replicas = 3;                 # or "auto" → min(3, nodeCount)
    strategy = "primary-replica";  # | "active-active" | "singleton" | "daemonset"

    resources = {
      cpu = "2";                  # cores, may be fractional
      memory = "4Gi";
      storage = { size = "100Gi"; class = "fast"; replication = 3; };
    };

    placement = {
      antiAffinity = "node";      # never 2 replicas on one node
      nodeSelector = { "disk" = "ssd"; };
      tolerations = [ ];
    };

    network = {
      ports = [ { name = "pg"; port = 5432; protocol = "tcp"; expose = "vip"; } ];
      healthCheck = { type = "exec"; command = "pg_isready"; period = "10s"; };
    };

    backup = { schedule = "0 2 * * *"; retention = "30d"; target = "default"; };

    config = { maxConnections = 200; sharedBuffers = "1GB"; };
  };
}
```

### 6.2 Block Runtimes

| Runtime | Use | Isolation | Startup |
|---|---|---|---|
| `systemd` | Native NixOS services (Postgres, Samba, nginx) | namespaces + cgroups + systemd hardening | ~0.1 s |
| `microvm` | Untrusted/foreign workloads, full VMs | cloud-hypervisor/QEMU | ~1 s |
| `oci` | Existing container images | podman, rootless | ~0.5 s |

Default is `systemd` because it is the lightest and most Nix-native. A block author picks per-block.

### 6.3 Block Lifecycle State Machine

```
  Defined ──validate──► Scheduled ──place──► Provisioning
                                                  │
                              ┌───────────────────┤
                              ▼                   ▼
                          Running ◄──heal──   Degraded
                              │                   │
                        ┌─────┼──────┐            │
                        ▼     ▼      ▼            ▼
                    Updating Stopping Failed ◄────┘
                        │     │
                        └──►  Terminated
```

Every transition emits an event to `/events/` with actor, reason, and generation.

### 6.4 Block Catalog (v1 shipped blocks)

**Storage:** smb-share, nfs-export, iscsi-target, minio (S3)
**Database:** postgresql, mariadb, redis, sqlite-litefs, clickhouse
**Web:** nginx, caddy, static-site, reverse-proxy
**AI:** ollama, llama-cpp-server, vllm, open-webui, embeddings/qdrant
**Virtualization:** microvm (generic VM), windows-vm
**Platform:** backup-agent, monitoring (VictoriaMetrics + Grafana), log-aggregator (Loki-compatible), certificate-manager (ACME), identity (LLDAP + OIDC)

### 6.5 Block SDK (extendability pillar)

A third-party block is a flake output:

```nix
outputs.expanseBlocks.my-block = {
  schema = ./schema.cue;          # config validation
  module = ./module.nix;          # NixOS module producing the runtime unit
  controller = ./controller.go;   # optional: custom reconcile hooks (plugin via gRPC)
  metadata = { name, version, description, icon, capabilities };
};
```

Custom controllers run as **out-of-process gRPC plugins** (like Terraform providers), so a buggy third-party block cannot crash `expansed`.

---

## 7. High Availability Model

### 7.1 Quorum Rules

| Node Count | Raft Quorum | HA Status | Behavior |
|---|---|---|---|
| 1 | 1 | None | Single node mode; all features work, no redundancy; UI shows persistent warning |
| 2 | 2 | Partial | Data replicated; control plane cannot tolerate a loss → use witness |
| 3 | 2 | **Full** | Tolerates 1 failure. Recommended minimum. |
| 4 | 3 | Full | Tolerates 1 failure |
| 5+ | ⌈n/2⌉ | Full | Tolerates ⌊(n-1)/2⌋ failures |

**Witness node:** a 2-node cluster can add a tiny witness (Raspberry Pi, VM, or cloud container) that participates in Raft but hosts no workloads. Makes 2+witness = 3-node semantics.

### 7.2 Failure Response Matrix

| Failure | Detection | Response | Target RTO |
|---|---|---|---|
| Process crash | systemd | restart, backoff | < 5 s |
| Block unready | health probe | restart, then reschedule | < 30 s |
| Node unresponsive | memberlist | mark suspect → dead, evict placements | < 15 s |
| Node dead (hard) | Raft + memberlist | reschedule all blocks, promote storage replicas | < 60 s |
| Leader loss | Raft election | new leader | < 10 s |
| Disk failure | SMART + I/O errors | mark degraded, rebuild replica elsewhere | < 5 min start |
| Network partition | Raft | minority side goes read-only, fences VIPs | immediate |
| Split-brain risk | Raft lease | minority cannot own VIP or be storage primary | prevented |

### 7.3 Anti-Split-Brain Invariant

> **No node may own a VIP, act as storage primary, or apply cluster config unless it holds a valid, unexpired Raft-issued lease.**

Leases are 5 s with 2 s renewal. A partitioned node self-fences within 5 s. This is the single most important safety property; Phase 03 and Phase 05 must include chaos tests that specifically try to violate it.

---

## 8. Web Management Interface

### 8.1 Technology

- Go `html/template`, server-rendered.
- **HTMX 2.x** for interactivity (`hx-get`, `hx-post`, `hx-swap`).
- **SSE** (`/events/stream`) for live status — not WebSockets, simpler and proxy-friendly.
- Tailwind CSS, compiled at build time, embedded via `go:embed`.
- **Zero runtime JS build step.** Total JS payload target: < 30 KB (htmx + sse ext).
- Progressive enhancement: every action has a non-JS `<form>` fallback.

### 8.2 Information Architecture

```
/                      Dashboard — cluster health, capacity, alerts, recent events
/nodes                 Node list → /nodes/:id (hardware, blocks, metrics, drain/cordon)
/blocks                Block list → /blocks/:id (status, replicas, logs, config, scale)
/blocks/new            Catalog browser → guided deploy wizard
/storage               Volumes, pools, replication health → /storage/shares (SMB/NFS/iSCSI)
/network               VIPs, DNS, firewall, LB backends
/ai                    Model catalog, running endpoints, OpenAI-compatible key mgmt
/backup                Targets, schedules, snapshot browser, restore wizard
/settings              Cluster config, users/roles, updates, generations + rollback
/events                Audit log, filterable
/terminal              Web console (per node, per block) — xterm.js over SSE/WS
```

### 8.3 UX Principles

1. **Every destructive action names the blast radius.** "This will stop 3 replicas serving 2 shares."
2. **Health is always visible.** Persistent top bar: cluster state, quorum, degraded count.
3. **No spinners without progress.** Long operations stream step-by-step progress via SSE.
4. **Everything the UI does, the CLI can do, using the same API.** No UI-only endpoints.
5. **Show the Nix.** Any screen can reveal the generated declarative config ("View as code").

---

## 9. AI / LLM Subsystem

Per PREMISE: on-prem LLMs with OpenAI-compatible access.

- **Model registry.** Curated catalog with size/VRAM requirements; models stored as content-addressed blobs on replicated volumes, deduplicated.
- **Inference blocks.** `ollama` (easy), `llama-cpp-server` (CPU/tuned), `vllm` (GPU throughput).
- **Unified gateway.** A single `/v1/*` OpenAI-compatible endpoint on the cluster VIP that routes by model name to the right backend, handles API keys, rate limits, and per-key usage accounting.
- **GPU scheduling.** Nodes advertise GPUs as resources; scheduler does exclusive GPU assignment with VRAM-aware bin packing; CPU fallback when no GPU is free.
- **HA for inference.** Stateless → N replicas behind the gateway, round-robin with least-outstanding-requests.

---

## 10. Backup & Disaster Recovery

### 10.1 What Gets Backed Up

1. **Cluster configuration** — all generations, serialized as a signed Nix expression bundle.
2. **Secrets** — sealed blobs (recoverable only with the recovery key, which is printed/QR'd at install and never stored in the backup).
3. **Block data** — ZFS snapshots, incremental.
4. **Metadata** — enough to rebuild placement and identity.

### 10.2 Mechanism

- **restic** for deduplicated, encrypted, incremental backups to S3/B2/Azure/local/SFTP.
- ZFS snapshot → `zfs send` → restic pipeline for application-consistent images.
- **Pre/post hooks per block** (e.g., `pg_start_backup`) so databases are consistent, not crash-consistent.
- **Backup verification job:** monthly automated restore of a random snapshot into a scratch microvm + integrity check. An unverified backup is reported as a warning, not a success.

### 10.3 Recovery Modes

| Mode | Command | Use |
|---|---|---|
| Block restore | `expansectl restore block <id> --at <ts>` | Oops, dropped a table |
| Node rebuild | boot ISO + `--rejoin <cluster>` | Replaced a dead machine |
| Full DR | boot ISO + `--restore-from <repo> --key <recovery-key>` | Cluster destroyed |
| Config rollback | `expansectl generation rollback` | Bad config, data intact |

**DR drill target: full cluster restore from bare metal in < 60 minutes for 1 TB.**

---

## 11. Security Model

- **Identity:** every node has an X.509 identity from the cluster CA, stored in TPM 2.0 when available. Join requires a short-lived token (15 min, one-time).
- **Secrets:** age/sops-encrypted at rest in Raft; decrypted only in-memory by the consuming block's unit via systemd credentials. Never written to `/nix/store` (world-readable).
- **AuthN:** local users (argon2id) + OIDC; WebAuthn/passkeys supported; TOTP fallback.
- **AuthZ:** RBAC — roles `viewer`, `operator`, `admin`, `owner`, scoped by block namespace.
- **Hardening:** systemd sandboxing on every unit (`ProtectSystem=strict`, `NoNewPrivileges`, seccomp), auditd, nftables default-deny, AppArmor profiles for microvms.
- **Supply chain:** flake inputs pinned by hash; release artifacts signed (minisign); SBOM generated per release; reproducible build verification in CI.

---

## 12. Observability

- **Metrics:** VictoriaMetrics (single binary, ~10x lighter than Prometheus at same retention). Node, block, storage, and network exporters built into `expansed`.
- **Logs:** journald → built-in shipper → VictoriaLogs, queryable from UI, 7-day default retention.
- **Traces:** OpenTelemetry from the control plane only (not workloads) — enough to debug reconcile latency.
- **Alerts:** built-in rule set (quorum lost, replica degraded, disk >85%, backup failed/unverified, cert expiring, node down). Delivery: webhook, email, ntfy, Matrix.
- **Self-diagnostics:** `expansectl doctor` runs 40+ checks and prints actionable remediation.

---

## 13. Performance Budgets (measurable, must be enforced in CI)

| Metric | Budget |
|---|---|
| `expansed` idle RSS | ≤ 120 MB |
| `expansed` idle CPU (3-node, 10 blocks) | ≤ 2% of one core |
| Base install disk footprint | ≤ 6 GB |
| Minimum viable node | 2 cores, 2 GB RAM, 20 GB disk |
| Boot → cluster ready | ≤ 90 s |
| UI first contentful paint (LAN) | ≤ 300 ms |
| UI JS payload | ≤ 30 KB gzipped |
| Reconcile loop tick (100 blocks) | ≤ 2 s p99 |
| API read p99 | ≤ 50 ms |
| API write (Raft commit) p99 | ≤ 200 ms |
| Failover (node death → service restored) | ≤ 60 s |
| exvol write overhead vs local | ≤ 25% |

---

## 14. Repository Layout

```
expanse/
├── flake.nix                   # top-level flake: packages, nixosModules, ISOs, checks
├── cmd/expanse/                # single binary, subcommands: agent|ui|proxy|ctl
├── internal/
│   ├── agent/                  # node agent, reconcile loop
│   ├── cluster/                # raft, memberlist, leases, CA
│   ├── store/                  # kv abstraction over raft FSM, generations
│   ├── scheduler/              # placement, constraints, bin packing
│   ├── blocks/                 # block engine, runtimes, lifecycle
│   ├── storage/                # exvol, zfs, ceph driver
│   ├── network/                # wireguard, vip, dns, nftables
│   ├── proxy/                  # L4/L7 LB
│   ├── api/                    # gRPC + REST handlers
│   ├── ui/                     # templates, handlers, static (go:embed)
│   ├── backup/                 # restic + zfs orchestration
│   ├── ai/                     # model registry, openai gateway
│   ├── observe/                # metrics, logs, alerts
│   └── secrets/                # age/sops, TPM
├── nix/
│   ├── modules/                # NixOS modules (expanse.* options)
│   ├── blocks/                 # shipped block definitions
│   ├── installer/              # ISO, kexec, disko layouts
│   └── overlays/
├── proto/                      # gRPC definitions
├── test/
│   ├── nixos/                  # NixOS VM integration tests
│   ├── chaos/                  # fault injection suites
│   └── e2e/                    # multi-node scenarios
└── docs/
```

---

## 15. Implementation Phases

Phases are ordered by dependency. Each has its own detailed spec file. **A phase is not complete until its exit criteria are green in CI.**

| Phase | Title | Outcome | Est. |
|---|---|---|---|
| [00](./PHASE00.md) | Foundation & Tooling | Repo, flake, CI, single binary skeleton, VM test harness | 2 wk |
| [01](./PHASE01.md) | NixOS Base Image & Installer | Bootable ISO, disko+impermanence, first boot, ZFS | 3 wk |
| [02](./PHASE02.md) | Node Agent & Local State | `expansed` runs, local KV, reconcile loop v1, nix driver | 3 wk |
| [03](./PHASE03.md) | Cluster Formation & Raft | Multi-node join, Raft store, leases, mTLS CA, generations | 4 wk |
| [04](./PHASE04.md) | Block Engine & Scheduler | Block spec, placement, systemd runtime, lifecycle, first real blocks | 4 wk |
| [05](./PHASE05.md) | Networking, VIP & Load Balancer | WireGuard mesh, VIP failover, `expanse-proxy`, DNS, firewall | 4 wk |
| [06](./PHASE06.md) | Storage Engine (exvol) | Replicated volumes, ZFS integration, failover, resync | 5 wk |
| [07](./PHASE07.md) | Network Storage Services | SMB, NFS, iSCSI blocks with HA | 3 wk |
| [08](./PHASE08.md) | Web UI & API | Full HTMX UI, REST API, SSE, auth, RBAC | 5 wk |
| [09](./PHASE09.md) | Virtualization & OCI Runtimes | microvm + podman block runtimes, live migration | 4 wk |
| [10](./PHASE10.md) | Databases & Service Blocks | Postgres/MariaDB/Redis HA, web blocks, cert manager | 4 wk |
| [11](./PHASE11.md) | AI / LLM Subsystem | Model registry, inference blocks, OpenAI gateway, GPU sched | 4 wk |
| [12](./PHASE12.md) | Backup, Restore & DR | restic+ZFS pipeline, verification, full DR restore | 4 wk |
| [13](./PHASE13.md) | Observability & Alerting | Metrics, logs, alerts, dashboards, `doctor` | 3 wk |
| [14](./PHASE14.md) | Security Hardening & Identity | TPM, RBAC depth, OIDC, audit, CIS-style benchmark | 3 wk |
| [15](./PHASE15.md) | Block SDK & Ecosystem | Plugin ABI, registry, docs, example third-party blocks | 3 wk |
| [16](./PHASE16.md) | Scale, Chaos & Release 1.0 | 16-node scale test, chaos suite, perf budgets, docs, GA | 4 wk |

**Total: ~62 weeks single-track; ~28–32 weeks with 3 parallel streams after Phase 05.**

### 15.1 Dependency Graph

```
00 ─► 01 ─► 02 ─► 03 ─► 04 ─┬─► 05 ─┬─► 06 ─► 07 ─┐
                             │       │              │
                             │       └─► 09 ────────┤
                             │                      │
                             └─► 08 ────────────────┼─► 16
                                                    │
                    04,05,06 ─► 10 ─────────────────┤
                    04,05 ────► 11 ─────────────────┤
                    03,06 ────► 12 ─────────────────┤
                    02,03 ────► 13 ─────────────────┤
                    03,08 ────► 14 ─────────────────┤
                    04 ───────► 15 ─────────────────┘
```

### 15.2 Milestones

| Milestone | After Phase | Demonstrable Capability |
|---|---|---|
| **M1 — It Boots** | 01 | ISO installs a reproducible NixOS node |
| **M2 — It Clusters** | 03 | 3 nodes form a quorum, survive a kill |
| **M3 — It Runs Things** | 05 | Deploy nginx, reach it on a VIP, kill a node, stays up |
| **M4 — It Stores Things** | 07 | Replicated SMB share survives node loss |
| **M5 — It's Usable** | 08 | Non-expert deploys a block from the web UI |
| **M6 — It's Complete** | 13 | All PREMISE features present |
| **M7 — It's Trustworthy** | 16 | Chaos suite green, perf budgets met, 1.0 |

---

## 16. Cross-Cutting Engineering Standards

Every phase must comply.

**Code**
- Go 1.23+, `golangci-lint` clean, `gofumpt` formatted.
- No `panic` in library code; errors wrapped with context.
- Every exported symbol documented.
- Unit test coverage ≥ 70% overall, ≥ 85% for `cluster`, `store`, `storage`.

**Nix**
- All inputs pinned; `nix flake check` passes.
- Every NixOS option has `description`, `type`, `default`, and `example`.
- No IFD (import-from-derivation) in the critical path.

**Testing pyramid**
1. Unit tests (fast, hermetic).
2. NixOS VM tests (`nixosTest`) — real systemd, real network.
3. Multi-node e2e (3–5 VMs).
4. Chaos (fault injection).
5. Performance regression against §13 budgets.

**Definition of Done for any feature**
- [ ] Implemented + unit tested
- [ ] NixOS VM test covering happy path and one failure path
- [ ] Exposed via API **and** CLI **and** UI (or explicitly justified)
- [ ] Metrics + events emitted
- [ ] Docs page written
- [ ] Performance within budget
- [ ] Upgrade path from previous version verified

**Versioning & compatibility**
- SemVer. Cluster supports N and N-1 node versions simultaneously (rolling upgrade).
- State schema migrations are forward-only, versioned, and tested with real old-state fixtures.

---

## 17. Key Risks & Mitigations

| # | Risk | Impact | Mitigation |
|---|---|---|---|
| R1 | Custom storage replication (`exvol`) has subtle data-loss bugs | Critical | Formal invariants + property-based testing + jepsen-style linearizability checks; ship Ceph as the escape hatch; long soak tests before 1.0 |
| R2 | NixOS learning curve deters contributors | High | Block SDK hides Nix; generate Nix from higher-level specs; extensive examples |
| R3 | Split-brain despite leases | Critical | Chaos tests dedicated to partition scenarios; fencing verified in Phase 05 and re-verified in 16 |
| R4 | Scope creep (17 phases is a lot) | High | Milestones M1–M5 are independently useful; ship early, phases 09–15 can be post-1.0 if needed |
| R5 | Performance budgets missed late | Medium | Budgets enforced in CI from Phase 02, not at the end |
| R6 | Nixpkgs churn breaks builds | Medium | Pin to stable release channel; automated weekly update PR with full test suite |
| R7 | Hardware diversity (10-yr-old laptops) | Medium | Hardware compatibility test matrix; graceful degradation (no TPM, no ZFS-capable RAM, no AES-NI) |
| R8 | Live migration complexity | Medium | Ship cold migration first; live migration is a Phase 09 stretch goal |

---

## 18. Open Design Questions (resolve before the phase that needs them)

| Q | Question | Needed by |
|---|---|---|
| Q1 | Is `exvol` block-level or should it be ZFS-native (zrepl + sync send)? | Phase 06 |
| Q2 | Do we embed Raft in `expansed` or run a separate `expanse-store`? | Phase 03 |
| Q3 | Single binary vs. split binaries for memory isolation? | Phase 02 |
| Q4 | CUE vs. JSON Schema vs. Nix types for block config validation? | Phase 04 |
| Q5 | Live VM migration in 1.0 or 1.1? | Phase 09 |
| Q6 | Ship our own DNS server or embed CoreDNS as a library? | Phase 05 |
| Q7 | Upgrade model: rolling per-node or atomic cluster-wide generations? | Phase 03 |

---

## Appendix A — Glossary

| Term | Meaning |
|---|---|
| **Block** | Declarative unit of workload; the Lego brick |
| **Generation** | Immutable snapshot of full cluster desired state |
| **Placement** | Binding of a block replica to a node |
| **exvol** | Expanse's built-in replicated volume engine |
| **Lease** | Raft-issued, time-bounded right to act (own VIP, be primary) |
| **Witness** | Raft voter that hosts no workloads |
| **Impermanence** | Root FS wiped on boot; only `/persist` survives |
| **Reconcile** | Loop that drives observed state toward desired state |
| **Fencing** | Preventing a partitioned node from acting authoritatively |
