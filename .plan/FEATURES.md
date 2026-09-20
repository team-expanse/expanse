# Features

Every feature in [`PREMISE.md`](./PREMISE.md), its honest status, and the component that delivers
it. **Status is assessed against working, tested behaviour — not against code existing.**

Status vocabulary: **Done** (works, has VM-test coverage) · **Partial** (some of it works) ·
**Rebuilding** (existed, being replaced) · **Not started**.

---

## 1. Control plane in Go and HTMX

**Partial.** The Go control plane is built and tested: agent, reconcile loop, gRPC API, CLI,
cluster substrate. **HTMX: zero lines exist.** The management interface has never been started —
see feature 8, which is the same gap.

## 2. Simple to install

**Done, needs rework.** Bootable ISO, hardware detection, TUI and unattended install, declarative
partitioning via disko, identity generation, impermanence verification. VM tests:
`install-unattended`, `install-refuses-dirty-disk`, `impermanence`, `boot-time`, `identity`.

*Rework required:* disk layouts are ZFS-based (`single`/`mirror`/`raidz1`) and must become
btrfs-system + LVM-data per `ARCHITECTURE.md` §3.

## 3. Easy clustering (plug and play)

**Done.** mDNS discovery, HMAC join tokens, Raft-replicated state, membership gossip, cluster CA
with mTLS, generations and rollback, node lifecycle (cordon/drain/remove), witness nodes. VM tests:
`cluster-form`, `cluster-join-security`, `cluster-leader-failover`, `cluster-node-loss`,
`cluster-partition`, `cluster-full-restart`, `cluster-witness`, `cluster-generations`,
`cluster-linearizable`. Chaos suite covers kill, partition storm, clock skew, slow disk, packet
loss and leader churn.

## 4. Network-based storage — SMB, NFS, iSCSI

**Not started.** All three depend on the volume layer (feature 7), which is being rebuilt.
Planned components: Samba, nfs-kernel-server, LIO. See `EXTERNAL-COMPONENTS.md` §3.

## 5. Self-hosted services — web, database, on-prem LLMs

**Partial.** The block engine that runs them is built and tested: schema, catalog, JSON Schema
validation, deterministic scheduler, lifecycle state machine, systemd runtime with cgroups and
sandboxing, health probing, log streaming. Shipped block definitions: `nginx`, `static-site`,
`whoami`, `redis`, `ollama`, `node-exporter`. VM tests: `block-deploy`, `block-scale`,
`block-reschedule`, `block-rolling-update`, `block-antiaffinity`, `block-daemonset`,
`block-singleton`, `block-delete`, `block-catalog`.

*Missing:* an HA database block (Postgres with streaming replication) and an OpenAI-compatible LLM
endpoint surface. Ollama exists as a definition, not as an integrated, load-balanced endpoint.

## 6. Virtualized workloads

**Not started.** Planned as a block runtime alongside systemd, using QEMU/KVM via microvm.nix or
cloud-hypervisor, with disks on replicated volumes.

## 7. Always-redundant architecture

**Partial — the storage half is being rebuilt.**

| Sub-claim | Status |
|---|---|
| Everything is load-balanced | **Done** — L4/L7 proxy, health-checked pools, `net-lb-*` VM tests |
| Everything is highly available | **Done for services** — placement, anti-affinity, rescheduling, VIP failover |
| No single point of failure | **Done for the control plane** — Raft quorum, lease arbitration, `net-vip-no-duplicate` |
| Everything is replicated (real time) | **Rebuilding** — `exvol` scrapped; DRBD 9 on LVM replaces it |
| Any member can go down with virtually no disruption | **Partial** — true for services, pending for stateful volumes |

The redundancy *policy* (scheduler refuses single-replica placement at N≥3) is built and is a
differentiator we keep. The redundancy *mechanism* for block storage is what changed.

## 8. Powerful, yet intuitive web management interface

**Not started.** No HTML templates, no HTMX, no UI server. This is the largest gap between the
premise and reality, and the reason the product is currently invisible to a non-technical user.
Scheduled early in `ROADMAP.md` rather than late.

## 9. Cloud-based backups and restores — data *and* configs

**Not started.** Planned: restic or kopia to S3-compatible storage, snapshotting at the appropriate
layer (btrfs subvolume for configs and shares; LVM thin for opaque volumes). Config backup must
cover cluster generations and sealed secrets, so a cluster can be rebuilt from backup credentials
plus one command.

## 10. Lightweight — 10-year-old laptops to modern servers

**Holding, now measured differently.** Budgets are restated as whole-node outcomes in
`ARCHITECTURE.md` §8 so that adopting an external process or kernel module is judged on total cost
rather than Go binary size. The ZFS removal directly serves this: no ARC competing with workloads
on a 4 GB machine.

*Unverified:* the 4 GB / 2-core / single-disk target has not yet been validated end to end. That
becomes a standing gate in the roadmap.

---

## Summary

| # | Feature | Status |
|---|---|---|
| 1 | Go + HTMX control plane | Partial — Go done, HTMX absent |
| 2 | Simple to install | Done, needs storage-layout rework |
| 3 | Easy clustering | **Done** |
| 4 | SMB / NFS / iSCSI | Not started |
| 5 | Web / DB / LLM services | Partial — engine done, key blocks missing |
| 6 | Virtualized workloads | Not started |
| 7 | Always-redundant | Partial — services done, storage rebuilding |
| 8 | Web management UI | **Not started** |
| 9 | Cloud backup & restore | Not started |
| 10 | Lightweight | Holding, target unvalidated |

Three of ten done. The substrate is strong; the product surface is thin. The roadmap is ordered to
correct that.
