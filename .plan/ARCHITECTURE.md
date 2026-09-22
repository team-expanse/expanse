# Expanse — Architecture

**Scope of truth:** [`PREMISE.md`](./PREMISE.md) defines *what Expanse is*. This document defines
*how it is built*. Where the two disagree, the premise wins and this document is wrong.

**Companion documents:** [`EXTERNAL-COMPONENTS.md`](./EXTERNAL-COMPONENTS.md) (what we adopt and
why), [`FEATURES.md`](./FEATURES.md) (premise features and their status),
[`ROADMAP.md`](./ROADMAP.md) (phases), [`DESIGN-AUDIT.md`](./DESIGN-AUDIT.md) (the evidence behind
the 2026-09 redesign).

---

## 1. Governing principle

> **Data plane: adopt. Control plane: build in Go.**

Expanse's value is NixOS determinism, Lego-brick composability, redundancy by default, and one
coherent management surface. It is **not** distributed-systems internals. Block replication,
consensus, filesystems, packet filtering, service supervision and virtualization are commodity
infrastructure with decades of hardening elsewhere. We orchestrate them; we do not rewrite them.

Every component we *do* write must answer: *which existing project does this, and why doesn't it
fit?* That answer is recorded in `EXTERNAL-COMPONENTS.md` §3 before the code is written.

This principle is not aspirational — it was adopted after a build-vs-adopt audit found that one
custom subsystem (`exvol`, a from-scratch block replicator) accounted for 44% of all defect fixes
in project history while running at 0.16–0.37× the throughput of a local disk. See
`DESIGN-AUDIT.md`.

### 1.1 What this replaces

The previous design documents (`PRODUCT_DESIGN.md`, `PHASE00–16.md`) invented constraints the
premise never asked for — a "one binary policy", "no external daemons", and footprint figures
stated as design mandates. Those constraints are what forced the custom builds. They are withdrawn.
The premise asks for a **control plane** in Go and HTMX, and for the system to be **lightweight**.
Neither requires that the data plane be our own code.

---

## 2. Layer model

```
┌──────────────────────────────────────────────────────────────┐
│ L5  Web UI (Go html/template + HTMX, SSE) + CLI              │  ours
├──────────────────────────────────────────────────────────────┤
│ L4  Blocks — schema, catalog, scheduler, lifecycle           │  ours (the product)
├──────────────────────────────────────────────────────────────┤
│ L3  Orchestration — placement, health, failover, generations │  ours
├──────────────────────────────────────────────────────────────┤
│ L2  Cluster substrate — Raft state, leases, membership, CA   │  ours, on hashicorp/raft
├──────────────────────────────────────────────────────────────┤
│ L1  Node runtime — agent, systemd, DRBD, LVM, WireGuard, nft │  thin glue over adopted tools
├──────────────────────────────────────────────────────────────┤
│ L0  NixOS base — flake, modules, btrfs, impermanence         │  adopted
└──────────────────────────────────────────────────────────────┘
```

Rule of thumb: **L0–L1 is mostly other people's code. L2–L5 is mostly ours.**

---

## 3. Node storage stack

This is the 2026-09 redesign and the highest-consequence decision in the document.

### 3.1 Disk layout

**Single disk (the 10-year-old laptop case):**

```
GPT ─┬─ ESP (FAT32)                → /boot
     ├─ btrfs partition             → subvolumes @root @nix @persist @log
     └─ LVM PV → VG "expanse"       → thin pool → thin LVs (DRBD backing devices)
```

**Multi-disk server:**

```
system   : btrfs RAID1 across 2 disks   (self-healing; see 3.4)
data     : remaining disks as LVM PVs in VG "expanse"; vgextend as disks are added
```

The system volume is a **plain partition, not an LV**. This keeps LVM out of initrd entirely —
early boot needs btrfs and nothing else, which removes a class of failure from the hardest place
to debug. LVM activates later, in normal userspace, and only for the data pool.

### 3.2 Why btrfs for the system

In-tree (no kernel gate), checksums on the data that matters most (configs, secrets, Raft log),
subvolume snapshots for impermanence, transparent compression, and — with RAID1 — **self-healing**.
Never use btrfs RAID5/6.

**Impermanence** is implemented by wiping the `@root` subvolume at boot. Anything that survives a
reboot lives in `@persist` by construction, which is how determinism is mechanically enforced
rather than merely promised.

### 3.3 Why LVM for volume backing

DRBD requires a **raw block device** per volume, and btrfs cannot provide one — it has no zvol
equivalent. A raw file on btrfs would need `nodatacow`, which disables checksums for that file
(losing the only reason to use btrfs there) and still carries CoW fragmentation risk.

LVM thin LVs are proper device-mapper devices: fast, snapshot-capable, dynamically created,
resized and deleted online, and the backing store every piece of DRBD documentation assumes.
Raw partitions were evaluated and rejected — volume creation would mean repartitioning a live
system across three coordinated nodes, with no online resize and no snapshots.

**Thin pool policy — the exhaustion mode must not fire by accident:**

- No overprovisioning by default; sum of volume sizes stays within pool size, admin opt-in to exceed.
- Reserve headroom for snapshots. Even with zero volume overprovisioning, a long-lived diverging
  snapshot can fill the pool.
- Monitor `Data%` **and** `Meta%` — metadata fills separately and is easy to forget.
- Verify DRBD passes discards down, or the pool only ever grows.

Thick LVs remain a supported fallback if thin's first-touch allocation cost proves material on the
oldest target hardware; the trade is losing block-level snapshots for opaque volumes.

### 3.4 Local redundancy

**Mirror the system volume on servers; do not add local RAID for data by default.** Once volumes
are replicated three ways across nodes by DRBD, local RAID under the data PV buys MTBF, not
correctness — losing a node's pool means that node resyncs from peers, a case that must work
regardless. Prefer JBOD/HBA passthrough so btrfs owns the system mirror: hardware RAID hides the
second copy from btrfs, leaving it able to *detect* corruption but not *repair* it. mdadm RAID1 has
the same limitation for the opposite reason — no checksums, so a scrub cannot tell which mirror is
right.

### 3.5 Volume stack

```
thin LV → DRBD 9 (protocol C, quorum majority) → filesystem or raw
                                                  ├─ btrfs  : shares we control (end-to-end checksums)
                                                  ├─ xfs/ext4: general
                                                  └─ raw    : iSCSI targets, VM disks
```

Promotion is gated by **both** the Raft lease and DRBD's own quorum. Two independent mechanisms
must agree before a node writes — the lease provides cluster-level arbitration, DRBD provides
storage-level arbitration, and neither alone is trusted.

Snapshots are taken at the **highest appropriate layer**: btrfs snapshots inside shares (crash
consistent at the filesystem level), `pg_basebackup`/WAL for Postgres, LVM thin snapshots only for
opaque volumes whose contents we don't control (iSCSI, VM disks), which need guest quiesce anyway.

### 3.6 Kernel module budget

**DRBD is the only out-of-tree module.** btrfs and LVM are in-tree. This is the point of the
redesign: ZFS (out-of-tree, CDDL) plus DRBD would have meant two modules gating every kernel
upgrade, in a product whose central pillar is deterministic, reproducible upgrades. ZFS is removed.

### 3.7 Where persistent data lives

Block persistent data lives on **replicated volumes**, never in local `/persist`. `/persist` holds
only the Raft log, secrets, node configs and logs — small, bounded and predictable. This is what
makes the fixed system/data split safe to size at install time.

---

## 4. Cluster substrate

- **Consensus:** `hashicorp/raft` + `raft-boltdb`. Not reinvented — this is an adopted library.
  Our FSM (~400 lines) encodes domain semantics: revisions, CAS, transactions, generations.
- **State store:** a versioned KV interface (`store.Store`) with revisions, CAS, txn, prefix
  watches. Implemented over Raft in production and BoltDB single-node, both passing one conformance
  suite. The interface is deliberately etcd-shaped so the substrate can be swapped if it ever earns
  its cost.
- **Leases:** singleton ownership (VIP holder, volume primary) via CAS with TTL and a skew guard
  band. This is the one piece of the substrate with a defect history and it gets a hardening pass
  in Phase 1.
- **Membership:** `memberlist` gossip for fast failure suspicion. Never acted on alone — anything
  destructive requires a quorum decision.
- **Identity & PKI:** Ed25519 cluster CA, node certificates, mTLS on all internal traffic, join
  tokens with HMAC. Scheduled for independent security review (see `ROADMAP.md`).
- **Generations:** every accepted desired-state change snapshots the full desired state; rollback
  is "make generation N−1 current", mirroring NixOS generations at cluster level.

**Open consolidation item:** node liveness is currently tracked three ways — memberlist gossip, the
`nodelc` failure monitor, and per-node liveness leases. Reduce to one source of truth with the
others derived.

---

## 5. Networking

All of this is thin policy over kernel primitives; none of it re-implements a protocol stack.

- **Overlay:** WireGuard mesh (kernel), peers programmed from store state via `wgctrl`.
- **VIPs:** lease-arbitrated holder with unsolicited ARP/NDP. Deliberately not VRRP — VRRP has no
  quorum and can split-brain; leases inherit Raft's safety.
- **Load balancing:** L4 splice proxy and L7 reverse proxy over `net/http/httputil`, with the
  routing table derived from store watches and swapped atomically.
- **DNS:** authoritative server for the cluster zone over `miekg/dns`, zone derived from store state.
- **Firewall:** nftables ruleset programmed via `google/nftables`.

> **Correction to a prior decision record:** the old D5.6 claimed a full nftables reload drops
> conntrack and breaks live connections. `nft -f` replaces a ruleset in a single atomic transaction
> and conntrack lives in a separate kernel table that rule replacement does not flush. The
> incremental-set-update implementation works and stays, but the claim must not be cited as
> precedent.

---

## 6. Blocks — the product abstraction

A **block** is the Lego brick: a declarative unit of workload (service, database, share, VM, LLM
endpoint) that snaps into a cluster and is replicated, balanced and healthy by default.

- **Schema & catalog** — block definitions with JSON Schema config validation, shipped as Nix.
- **Scheduler** — two-stage filter/score placement, fully deterministic (no rand, no map order, no
  wall clock). Redundancy policy lives here: refuse single-replica placement at N≥3.
- **Lifecycle** — a pure state machine; side effects belong to its callers.
- **Runtime** — systemd units with cgroup limits and sandboxing, generated as NixOS module
  fragments. systemd is the supervisor; we do not write one.
- **Health** — liveness, readiness and data-currency signals, all three required for "healthy".

This layer is where custom code is *justified*, because it is the product.

---

## 7. What we deliberately do not build

Service supervisor, init system, filesystem, block replication engine, consensus algorithm, VPN
protocol, DNS protocol stack, HTTP protocol stack, TLS implementation, container image format,
hypervisor, database, LLM inference engine, backup engine, metrics TSDB.

---

## 8. Outcome budgets

The old design fixed binary size and RSS as mandates, which distorted decisions. Budgets are now
stated as **whole-node outcomes**, so adopting an external process or kernel module is judged on
total cost rather than on Go binary size alone. Enforced by `test/perf/budgets.yaml`.

| Budget | Target |
|---|---|
| Three-node HA cluster runs on | 4 GB RAM, 2 cores, one disk per node |
| Total Expanse control-plane overhead per node, idle | ≤ 200 MB RSS, ≤ 3% of one core |
| Replicated volume vs local disk | ≥ 0.75 seq write, ≥ 0.95 seq read, ≥ 0.60 rand write |
| Volume failover (node hard-kill → writable) | ≤ 20 s |
| Acked-write loss under any fault | **Zero. Release blocker.** |
| Cluster forms (3 nodes, cold) | ≤ 30 s |

---

## 9. Decision record

| # | Decision | Rationale |
|---|---|---|
| A1 | Data plane adopts, control plane is ours | `DESIGN-AUDIT.md`; one custom data-plane component produced 44% of defects |
| A2 | DRBD 9 replaces `exvol` | 20 years of hardening vs. ~12 data-affecting defects found in ours; 2–4× faster in a like-for-like spike |
| A3 | btrfs for system, LVM thin for volume backing | Both in-tree; keeps DRBD as the only out-of-tree module |
| A4 | ZFS removed | Out-of-tree kernel gate + untuned ARC on 4 GB targets; nothing it provided is unavailable elsewhere |
| A5 | System on a plain partition, not an LV | Keeps LVM out of initrd |
| A6 | Keep `hashicorp/raft`; do not adopt etcd | Consensus was never reinvented; raftstore has zero defects; smaller footprint |
| A7 | Lease + DRBD quorum both gate promotion | Two independent arbiters; neither trusted alone |
| A8 | No local RAID for data by default | Cluster replication covers it; mirror the system volume instead |
| A9 | Thin pool never overprovisioned by default | Pool exhaustion is a 3am failure mode; admin must opt in |
| A10 | Block data on replicated volumes, not `/persist` | Keeps `/persist` bounded so the install-time split is safe |
| A11 | Web UI runs in-process inside `expanse agent`, not a separate binary (`PHASE-02-TASKS.md` D1) | Its handlers call the same in-process `internal/blocks`/`internal/storage` control-plane packages the CLI already uses; a second process would need its own IPC path or its own raft/store participation for no isolation gain `net/http` doesn't already give per-request |
| A12 | Web UI's external TLS is signed by the cluster's own CA, no client certificate required (`PHASE-02-TASKS.md` D5) | Consistent with every other endpoint's trust root and needs no operator action at install; unlike the internal `:7443` mTLS endpoint, the caller is a browser, not a cluster peer, so no CN-membership check applies |
| A13 | `htmx` (and its SSE extension) vendored as embedded static files, not loaded from a CDN (`PHASE-02-TASKS.md` D6) | Keeps the UI usable on an offline install, consistent with the ISO shipping firmware and an offline nixpkgs channel; both are Zero-Clause BSD |
| A14 | Web UI sessions live in the cluster store (`internal/web/auth`), not process memory (`PHASE-02-TASKS.md` D2) | A cookie issued by one node must still authorize a request a different node answers after the UI's VIP fails over (X7); in-memory sessions make that fail by construction |
| A15 | Password hashing is argon2id via already-vendored `golang.org/x/crypto/argon2` (`PHASE-02-TASKS.md` D3) | Current best practice for new work; no new dependency, `blake2b` (argon2's own dependency) vendored alongside it |
| A16 | The initial admin credential is generated once at first agent start and shown once in the log (`store.CompareAndSwap` expect-absent decides the single winner across racing nodes); reset is a local, `ctl`-trust-level operation (`PHASE-02-TASKS.md` D4) | Analogous to how a join token is minted and shown once; deliberately no "forgot password" flow, consistent with `ctl`'s existing local-socket trust model |
| A17 | CSRF is a double-submit cookie compared against the value in the session's store record, not a separate signing scheme (`PHASE-02-TASKS.md` D7) | Standard fit for HTMX's header-driven mutating requests; needs no new library and rides the same store-backed session that already answers D2 |
| A18 | Every node certificate carries a fixed shared SAN, `ca.UIVIPHostname` (`"expanse-ui"`), alongside its own node ID (`PHASE-02-TASKS.md` D9) | A client reaching the UI's VIP cannot know in advance which node will answer; a name every node's cert already carries gives a hostname match regardless of which one currently holds it, without weakening any other cert check |
