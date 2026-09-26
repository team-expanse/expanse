# Roadmap

Ordered by **user-visible outcome**, not by architectural layer. The previous plan sequenced
bottom-up through sixteen phases and reached phase six with no web interface, no share, no VM and
no backup — every headline feature in the premise still at zero. This roadmap corrects that: each
phase ends with something a person can see and use.

---

## Phase protocol

1. **Each phase opens with a task breakout.** The first activity is producing
   `PHASE-NN-TASKS.md` — the phase decomposed into tasks with explicit acceptance criteria. Task
   files are written at phase start, never in advance, because earlier phases change what later
   ones need.
2. **Each phase closes with a vertical slice**: a VM test that exercises the feature end to end
   *and kills a node while doing it*. A phase is not done because code exists.
3. **The adoption test (`EXTERNAL-COMPONENTS.md` §6) runs before any new subsystem.** An
   unanswered adoption test blocks the task.
4. **Budgets are gates, not reports.** `test/perf/budgets.yaml` is the single source of numbers.
5. **Any acked-write loss is a release blocker.** Fix the system, never the test.

---

## Phase 1 — Storage foundation

**Goal:** replicated volumes that survive a node hard-kill and meet perf budgets, built on DRBD 9
over LVM thin, with btrfs as the system filesystem.

- Remove `exvol` — surgically. The volume *control plane* (placement, spec/status model, CLI,
  controller), the `vol-*.nix` VM tests and the perf harness all transfer; only the replication
  engine goes.
- Replace `internal/storage/zfs` with an LVM wrapper (VG/thin pool/LV lifecycle).
- New disko layouts: btrfs system partition + LVM PV, single-disk and mirrored-system variants.
- Impermanence on a btrfs `@root` subvolume, replacing the ZFS snapshot rollback.
- DRBD resource generation and lifecycle driven from volume specs; promotion gated by **both** the
  Raft lease and DRBD quorum.
- Harden `internal/cluster/lease` — the renew loop that stopped permanently on a failed CAS, and
  the lost-CAS path. This is the one substrate component with a defect history.
- Swap the hand-written linearizability checker for **Porcupine**, and cross-check recorded
  histories before trusting the durability gate.
- Update `docs/INSTALL.md`, which documents the ZFS layout and becomes wrong the moment the new
  disko layouts land.

**Exit:** `vol-durability` green on DRBD (20 hard-crash iterations, **zero acked-write loss** — met, see PHASE-01-TASKS E2);
storage budgets met; online resize and snapshot/restore working; all three replicas checksum-equal
after every fault.

**Gate and fallback:** the durability loop decides this phase. If DRBD fails it, the fallbacks are
Ceph (opt-in at higher node counts, ruled out for the 4 GB laptop case) or pipelining a custom
write path — re-opened only with evidence, never by default.

## Phase 2 — Web management interface

**Goal:** a person operates the cluster without touching the CLI. Closes the largest premise gap.

- Go `html/template` + HTMX + SSE, served by every node, reached through a VIP.
- Cluster overview: nodes, health, quorum, generations, events.
- Blocks: list, deploy, scale, update, delete, logs.
- Volumes: create, resize, snapshot, replica state.
- Authentication and an initial admin credential established at install.

**Exit:** deploy nginx from the UI, then kill the node holding the UI VIP — the interface stays
reachable and the deployment completes — met, see PHASE-02-TASKS E1.

## Phase 3 — SMB and NFS

**Status: paused partway** (Samba deploy/colocation done; kill-mid-write failover blocked on an
unresolved bug — see `PHASE-03-TASKS.md` Stream B2). Independent of Phases 4–11 per this roadmap's
own Parallelism section below; resume when it's worth another look, no earlier phase depends on it.

**Goal:** premise feature 4, two of three.

- Samba block on a DRBD-backed volume; NFSv4.1 export block.
- HA approach (ctdb vs. active/passive over DRBD) decided at task breakout, with the grace-period
  and lock-state handling that failover actually requires.

**Exit:** a client mounts a share and writes continuously; the serving node is killed; writes
continue and the data verifies.

## Phase 4 — iSCSI

**Goal:** premise feature 4, third of three.

- LIO target over a raw DRBD volume, `SINGLETON` + VIP failover (not ALUA multipath — a DRBD
  Secondary refuses to open a backstore at all, ruling it out; see `PHASE-04-TASKS.md` D1),
  persistent reservations.

**Exit:** an initiator sustains I/O through a node kill with no corruption — met, see
`PHASE-04-TASKS.md` Stream D. (Persistent reservations do not currently survive that failover,
measured directly, non-blocking per the phase's own exit criteria — see Stream C/D and D4.)

## Phase 5 — Database and service blocks

**Goal:** premise feature 5 (web and database).

- PostgreSQL block with **streaming replication** — application-level HA, deliberately not block
  replication, which is the wrong layer for a database.
- Backup integration hooks for Phase 8.

**Exit:** `pgbench` runs through a primary kill; the new primary serves; `amcheck`/`pg_checksums`
reports zero corruption — met, see `PHASE-05-TASKS.md` Streams A-D.

## Phase 6 — Virtualized workloads

**Goal:** premise feature 6.

- VM block runtime — **plain QEMU/KVM, exec'd directly** (not microvm.nix, not cloud-hypervisor,
  which failed a real boot-compatibility test; see `PHASE-06-TASKS.md` D1) — alongside the systemd
  runtime, with no new runtime kind actually needed.
- VM disks on replicated volumes; cold migration first, live migration deferred (documented scope
  note, `docs/VMS.md` §6).

**Exit:** a running VM survives node loss by restarting on another node with its disk intact — met,
see `PHASE-06-TASKS.md` Streams A-D.

## Phase 7 — On-prem LLM subsystem

**Status: paused before any code** (task breakout and D1's adoption test done — Ollama's own
CPU/Vulkan/CUDA/ROCm builds confirmed available in nixpkgs, NPU confirmed to need from-scratch
driver packaging and deferred; see `PHASE-07-TASKS.md`). Paused on direct instruction: no accelerator
test hardware is available and this was future planning, not a current need. Independent of every
other phase per this roadmap's own Parallelism section below; resume whenever it's worth another
look, no other phase depends on it.

**Goal:** premise feature 5 (LLMs).

- Ollama / llama.cpp block exposing an OpenAI-compatible endpoint, load-balanced across replicas.
- GPU passthrough where present, CPU fallback where not.

**Exit:** an unmodified OpenAI-compatible client keeps working through a node kill.

## Phase 8 — Backup and restore

**Goal:** premise feature 9 — data *and* configs.

- restic or kopia to S3-compatible storage.
- Snapshot at the right layer: btrfs for configs and shares, LVM thin for opaque volumes.
- Cluster configuration, generations and sealed secrets included, not just data.

**Exit:** destroy the cluster entirely; rebuild from backup credentials plus one command; data and
configuration both verify.

## Phase 9 — Observability and alerting

- Prometheus-compatible metrics export, Grafana dashboards, alert rules on node, volume, block and
  quorum health. Adopt the TSDB; do not write one.

**Exit:** a node failure raises an alert and is visible in the UI and dashboards within 30 s.

## Phase 10 — Security hardening and identity

- **Independent security review of the cluster CA, join-token format and revocation registry**,
  carried forward from the design audit. Hand-rolled crypto plumbing reviewed by someone who did
  not write it.
- TPM sealing of the cluster secret, CA rotation, OIDC for the web interface.

**Exit:** review findings triaged and closed; CA rotation exercised in a VM test.

## Phase 11 — Scale, chaos and 1.0

- Validate the whole-node budget on real 4 GB / 2-core / single-disk hardware.
- Extended chaos soak across storage, cluster and network simultaneously.
- Documentation, upgrade path, release.

**Exit:** budgets met on target hardware, chaos suite green over a long soak, 1.0.

## Phase 12 — Single-node clusters (1.1)

- Default volumes and blocks work on one node; replication clamps to the nodes available and
  records its target.
- Volumes grow to their target automatically as nodes join; one node says "no redundancy".
- Optional: an installed node usable as a dev workstation (persisted `/home`, local config).

**Exit:** single-node and grow-on-join VM tests green, zero acked-write loss across 1 → 3, 1.1.0 — met, see `PHASE-12-TASKS.md` (the dev-workstation option deferred).

---

## Cross-cutting items

Picked up by whichever phase's task breakout reaches them first; none large enough to own a phase.

- **Consolidate node liveness.** Three mechanisms currently answer "is this node alive" —
  memberlist gossip, the `nodelc` failure monitor, and per-node liveness leases. Reduce to one
  source of truth with the others derived.
- **Reconcile managers vs. NixOS.** The `file`, `sysctl` and `systemd` resource managers converge
  state NixOS already converges declaratively. Audit which resources are genuinely runtime-dynamic
  (volume attach, VIP holder, placement) and delete the rest.
- **Correct the D5.6 record.** The claim that a full nftables reload drops conntrack is almost
  certainly false; verify and correct so it is not cited as precedent. The implementation stays.
- **Block data placement.** Ensure per-block persistent data lands on replicated volumes, never
  local `/persist` (`ARCHITECTURE.md` §3.7).

## Parallelism

Phases 3–7 depend on Phase 1 but not on each other, and Phase 2 depends on neither. With more than
one person, Phase 2 runs alongside Phase 1. Sequentially, the order above is the recommended one:
storage first because it is both the currently-broken thing and the blocker for four later phases;
the interface second because until it exists nobody can see the product.
