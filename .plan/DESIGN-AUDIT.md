# Design Audit — Build vs. Adopt (Phases 00–06)

**Status:** working draft for discussion. **Scope:** everything built or planned through Phase 06.
**Trigger:** the T23 perf run showed exvol (our replicated-volume engine) at 0.16–0.37× of local
for every fio profile, and a one-day DRBD 9 spike on the same VMs reached 0.62–1.06×. That raised
the question this document tries to answer for the rest of the tree: *where else did we write
something from scratch that a proven project already does?*

**How to read this.** Nothing here is a decision. Each item lists what we wrote, what could replace
it, the strongest argument *for* keeping ours, and the smallest experiment that would settle it.
Confidence ratings are my judgment from reading the code and plans; anything I did not verify is
marked **unverified**. Line counts are non-test Go, measured today (`wc -l`), total ≈ 39.5 k.

---

## 1. The frame: differentiator vs. commodity

Expanse's stated identity (PREMISE.md, PRODUCT_DESIGN §1) is *NixOS-native, block-composable,
redundant-by-default, one small binary*. The parts that make that true are ours to build:

| Differentiator (keep building) | Why it is ours |
|---|---|
| Block model, catalog, schema, Nix-module generation | The product's user-facing abstraction |
| Cluster-level generations / rollback semantics | Mirrors NixOS generations; the pitch for Scenario 3 |
| Redundancy policy (scheduler refuses single replica at N≥3, volume affinity) | The "by default" guarantee |
| Installer/UX glue, `doctor`, CLI, later the web UI | Product surface |

Almost everything underneath is *commodity infrastructure* with decades of production hardening
elsewhere: block replication, key-value consensus with leases, load balancing, DNS, VIP failover,
PKI, overlay networking, config rollout. Those are where custom code costs the most (defect rate,
security exposure, review burden) and earns the least.

**Pattern observed in the plans.** Decision records (D2.x–D5.x) justify "own X, not Y" with
footprint or simplicity arguments, none with a measured comparison, and the cost estimates were
optimistic: D5.1 estimated the DNS server at "~300 LOC"; it is 883 (+ tests). The linearizability
checker was written from scratch although PHASE06-TASKS T21 said to consider vendoring one. Risk
R1 in the product design named "ship Ceph as the escape hatch" for exvol; no equivalent exit
existed for the other components. **Recommendation: add a build-vs-adopt step to the plan template
before any new subsystem is started** (see §6).

## 2. Scorecard

Priority = expected value of investigating (avoided defect/maintenance cost × plausibility that a
proven option fits). "Evidence" says what we already know.

| # | Area | Our code | Replace with? | Priority | Evidence so far |
|---|---|---|---|---|---|
| A | exvol replicated volumes | 9.7 k prod + 6.8 k tests + chaos harness (`test/chaos` 6.5 k, all phases) | **DRBD 9** on the same zvols | **P0** | Measured (spike). ~a dozen data-affecting defects found in T21/T22 |
| B | Raft store + leases + watches | raftstore 1.9 k, lease 0.5 k, boltstore 0.4 k, generations 0.3 k | **Embedded etcd** (or Consul) behind `store.Store` | **P0** | Not measured; conformance suite makes a spike cheap |
| C | L4/L7 load balancer | proxy 1.1 k | **HAProxy / Envoy / Traefik / Caddy**; IPVS for L4 | **P1** | Not measured; D5.2 rationale is weak (see C) |
| D | Node config rollout, rollback, generations | agent nix driver 0.4 k, managers 1.0 k, generation 0.3 k | **deploy-rs / Colmena / Comin** | **P1** | Overlap with NixOS itself; two sources of truth |
| E | Scheduler + block runtime | scheduler 0.6 k, controller 1.3 k, runtime 0.7 k, service/wire/validate/health/logs/catalog + block-run ≈ 3.7 k | **Nomad** (or plain systemd + NixOS) | **P1** | Not measured; needs a feature-mapping pass |
| F | Cluster CA, join tokens, revocation | ca 0.4 k, join 0.7 k, control 1.1 k, nodelc 0.4 k | **step-ca / Vault PKI / SPIRE**; kubeadm-style bootstrap | **P1** (security) | Hand-rolled crypto plumbing; Phase 14 adds more |
| G | WireGuard overlay | mesh 0.9 k | **Nebula / Tailscale (Headscale) / Netbird**, or static NixOS `networking.wireguard` | P2 | Depends on NAT/roaming requirements |
| H | Cluster DNS | dns 0.9 k | **CoreDNS** plugin, or dnsmasq/Unbound + generated zone | P2 | Estimate was 3× off |
| I | VIP failover | vip 0.8 k | **keepalived** (unicast VRRP + lease-gated track script) | P2 | D5.3 has a valid point; cost unmeasured |
| J | Firewall | firewall 0.8 k | NixOS `networking.nftables` + `nft` sets | P2 | D5.6 premise is **unverified** (see J) |
| K | Liveness detection | membership 0.4 k, nodelc 0.4 k, per-node liveness leases | Consolidate (or Serf/Consul semantics) | P2 | Three overlapping mechanisms; internal duplication |
| L | Linearizability checker | checker 0.25 k | **Porcupine** (or Elle) | P2, cheap | Plan said to vendor; we wrote it |
| M | Inventory / health | inventory 0.4 k, health 0.5 k | **ghw / gopsutil** (or node_exporter, shipped in P13) | P3 | Low risk either way |
| N | Installer | install 0.9 k | disko (**already used**) + nixos-anywhere / `nixos-install` | P3 | Partly adopted; check remaining glue |
| O | Quantity parser | quantity 0.25 k | `k8s.io/apimachinery/pkg/api/resource`, `docker/go-units` | P3 | Small; likely fine |

## 3. Items that need digging

Each entry: **What we built → Candidates → Steelman for ours → Experiment (pass/fail) → Open risks.**

### A. exvol → DRBD 9 (P0)

- **What we built.** A userspace synchronous block replicator: primary coordinator with pipelined
  pumps, quorum ack, oplog, op-replay and snapshot resync, fencing, recovery/failover algorithm
  (probe, pull, level, stale), NBD device, controller-side election with currency gate. About 9.7 k
  lines plus ~6.8 k of tests and a chaos/linearizability harness.
- **Measured.** exvol vs raw zvol on the same node: seq write 0.31, seq read 0.36, rand write 0.16,
  rand read 0.37, fsync p99 26.9 ms (allowance 6.7 ms). DRBD 9 spike: 0.62, 1.01, 0.79, 1.06,
  6.8 ms; 256 MiB incremental resync in 9.8 s; crash→writable survivor 2.8 s; an acked block
  survived. Root cause of exvol's ratios: three serialization points (NBD loop is one request at a
  time; `Coordinator.replicate` holds a mutex across the quorum round trip; local reader shares the
  writer's lock). A pipelined rewrite is possible but touches the ordering-critical path.
- **Candidates.** DRBD 9 (in-kernel, quorum, auto-promote, bitmap resync; `pkgs.drbd` 9.2.16 builds
  for the pinned 6.18 kernel). Ceph RBD (already the planned Tier B; heavy). Longhorn / Mayastor
  (Kubernetes-oriented; user-space or SPDK; same or larger dependency).
- **Steelman for ours.** No out-of-tree kernel module; ZFS-checksummed data path end to end;
  resync via ZFS snapshots; Raft lease directly gates writes.
- **Experiment.** (1) `vol-drbd-durability`: 20-iteration hard-crash loop equivalent to
  `vol-durability` (acked-write loss = fail). (2) Split-brain behaviour under partition with
  `quorum majority` + our lease as the promote gate. (3) Online resize and snapshot/restore
  (`drbdadm resize`, zvol snapshots) against the T20 tests. (4) Repeat the perf run 3× for
  variance; try tuning for the two misses (seq write 0.62, fsync).
- **Open risks.** Kernel-module maintenance across NixOS upgrades (out-of-tree); our own
  sunk cost (do not let it decide); data-integrity story must be re-argued (DRBD
  `data-integrity-alg`, ZFS scrub underneath).

### B. Raft store, leases, watches → embedded etcd (P0)

- **What we built.** hashicorp/raft with a hand-written FSM, versioned command encoding, prefix
  watches with overflow handling, leader forwarding, linearizable reads, snapshots (raftstore
  1.9 k); a lease manager with CAS, TTL, skew allowance and guard bands (526); generations
  (321); node liveness leases; the Phase 03 plan itself says "highest-risk code in the product".
  We have been debugging this layer indirectly: the T22 fixes included lost lease CAS, renew loops
  that stopped permanently, and liveness leases expiring.
- **Candidates.** `go.etcd.io/etcd/server/v3/embed` provides exactly this surface: MVCC KV, txn/CAS,
  watches with revisions, **leases with keepalive**, linearizable reads, snapshots, learners,
  compaction/defrag, and a `concurrency` package (sessions, elections, mutexes). Consul (KV +
  sessions) is the other proven option but is a separate process. rqlite/dqlite are SQL, a worse
  fit.
- **Steelman for ours.** "No external etcd" is stated policy; binary is budgeted at 40 MiB and idle
  RSS at 120 MB; embedded etcd will add tens of MB (**unverified — measure**). Our lease uses wall
  clocks with an explicit skew guard band; etcd leases are also time-based, so this is not a clear
  differentiator.
- **Experiment (cheap, ~1–2 days).** A `store.Store` implementation over embedded etcd that passes
  the existing `store/conformance` suite. Measure binary size, idle RSS, 3-node write p99 against
  the `raft_*` budgets, and the lease semantics used by VIP and volume primaries. Pass = conformance
  green and budgets met with size headroom. The suite exists precisely so this is a drop-in test.
- **Open risks.** Lease API differs (etcd fencing by lease revision vs our term-based fencing
  metadata); watch semantics under compaction; upgrade/snapshot format lock-in.

### C. L4/L7 proxy → HAProxy / Envoy / Traefik / Caddy (P1)

- **What we built.** L4 splice proxy and L7 reverse proxy (on `net/http/httputil.ReverseProxy`),
  health-checked pools, atomic routing-table swap from store watches (1.1 k).
- **Candidates.** HAProxy (runtime API, seamless reload), Envoy (xDS is *literally* store-driven
  config with no reload), Traefik (dynamic providers), Caddy (admin API, embeddable Go), IPVS/LVS
  for L4 (kernel, connection-preserving).
- **Why D5.2's rationale is thin.** "No reload semantics, no config generation" is answered by Envoy
  xDS, Traefik providers, HAProxy's runtime API, and Caddy's admin API. "Single binary" is a
  footprint argument, not a correctness one.
- **Steelman for ours.** Footprint, one process, health/pool logic already integrated with store
  state; an L7 built on the standard library's `ReverseProxy` is not exotic.
- **Experiment (~1–2 days).** Drive HAProxy (or Envoy) from a generated config over the same fixture
  and compare against the existing budgets: `lb_rps`, `lb_added_latency_p99_us`, plus RSS and
  failover-during-backend-death behaviour. Also list what we would need to re-implement for
  Phase 07+ (TLS termination, HTTP/2, WebSocket, connection draining, rate limits) — the long tail
  is where hand-written proxies cost the most.
- **Open risks.** Extra process per node (RSS budget); GPL licence of HAProxy vs static-binary
  goal (**verify**); generation of config still needs a translator.

### D. Node config rollout / rollback → deploy-rs, Colmena, Comin (P1)

- **What we built.** An agent that drives `nix build`/`switch-to-configuration`, a pending-switch
  watchdog with auto-rollback (D2.6), a resource-manager reconcile loop with file, sysctl, systemd
  and nix-config managers (1.4 k together with the DAG engine), plus cluster generations in the
  Raft store.
- **Candidates.** deploy-rs ("magic rollback" is the same idea as D2.6), Colmena, Comin
  (pull-based GitOps for NixOS), NixOS's own `system.autoUpgrade` + `switch-to-configuration
  test`/boot-counting for rollback.
- **The design question underneath.** NixOS already *is* a declarative reconciler for files, sysctl
  and systemd units. We now have two desired-state systems (Nix modules and the Raft store) and a
  loop that converges host resources NixOS already converges. The persona in PRODUCT_DESIGN P2
  wants one `expanse.nix` in git; generations in Raft duplicate that.
- **Steelman for ours.** Cluster-wide, quorum-gated rollout and rollback across nodes is not what
  deploy-rs/Colmena give you; dynamic, non-Nix state (volume attach, VIP holder) still needs a
  runtime reconciler.
- **Experiment (paper first, ~0.5 day).** List every resource kind the managers handle, mark which
  are *runtime-dynamic* (need a loop) vs *static* (NixOS can own it), and compare rollback
  semantics of D2.6 vs deploy-rs. Then decide whether managers other than `nixman` and the
  dynamic ones should exist at all.

### E. Scheduler and block runtime → Nomad, or systemd + NixOS only (P1)

- **What we built.** Two-stage placement (P1–P11 predicates, S1–S6 scores, deterministic
  tie-breaks), leader-side placement controller, block lifecycle state machine, systemd runtime
  with cgroup/sandbox unit generation and closure cache, health probes, log streaming, validation
  rules V1–V24 (≈ 6.4 k total incl. `expanse-block-run`).
- **Candidates.** Nomad (constraints, spread, distinct-hosts, drain, rolling update, health checks;
  Raft-based; `exec`/`raw_exec` drivers). **Licence caveat:** Nomad moved to the source-available
  BSL 1.1 in 2023 — confirm this is acceptable. k3s/Kubernetes is a stated non-goal.
- **Steelman for ours.** The block model is the product; NixOS-closure-based deployment and
  volume-affinity placement are specific; Nomad brings its own state store (a second consensus
  group alongside ours unless we also adopt it in place of B).
- **Experiment (paper, ~1 day).** Map each predicate, score, lifecycle state and validation rule
  onto Nomad job-spec features; list what is left over. If the leftover is small, the block
  definition compiles to a Nomad job and B, E collapse into one adopted platform. If large, keep
  ours and stop asking.

### F. PKI, join tokens, revocation → step-ca / Vault PKI / SPIRE (P1, security)

- **What we built.** Ed25519 root CA with node cert issuance/renewal and a dual-CA trust bundle
  (433), a join token format (`expanse-join-<base58(clusterID‖expiry‖nonce‖HMAC)>`) and join
  service (653), an enrollment/control layer (1.1 k), a revocation registry and failure monitor
  (nodelc, 392). Phase 14 plans CA rotation and TPM sealing on top.
- **Candidates.** smallstep `step-ca` (ACME, JWK provisioner tokens, short-lived certs, rotation,
  Apache-2.0), Vault PKI, SPIRE for workload identity; kubeadm-style bootstrap tokens as a
  reference design for join.
- **Why this matters more than its size.** Hand-rolled crypto protocols and PKI are the class of
  code where review by the authors is least reliable. Even if we keep the code, it needs an
  independent review that we have not planned (PHASE06 has an "external review" item only for
  exvol).
- **Steelman for ours.** Zero extra processes; the CA lives in the Raft store; we control the
  bootstrap flow on a fresh 3-node cluster.
- **Experiment (~1 day).** (1) Independent security review of the token format and `ca` package.
  (2) Prototype `step-ca` as an embedded library or sidecar and list what enrollment would look
  like. Outcome: keep with a review, or replace the issuance/renewal core and keep only the
  Expanse-specific join UX.

### G. WireGuard overlay → Nebula / Tailscale / Netbird / static NixOS (P2)

- **What we built.** Per-node WireGuard identity, a reconciler that diffs peers from store state and
  applies them with `wgctrl`/`netlink`, full mesh with 25 s keepalive (927). D5.5: "full mesh, no
  relay, revisit above 50 nodes".
- **Candidates.** Nebula (lighthouses, NAT punching, MIT), Tailscale/Headscale (DERP relays, NAT
  traversal, ACLs), Netbird, Innernet; or generate `networking.wireguard.interfaces` from Nix.
- **Deciding question.** Are peers ever behind NAT, roaming, or on routed networks that need
  traversal? For "3 laptops on one LAN" our approach is adequate; the personas' P2 (routed
  networks, static seeds) suggests it will matter.
- **Experiment (paper, ~0.5 day).** State the NAT/roaming requirement in one sentence; if it is
  "none", record that and stop.

### H. DNS → CoreDNS plugin or dnsmasq/Unbound (P2)

- **What we built.** Authoritative server for `expanse.local` from store state, forwarding, cache,
  mDNS bridge on `miekg/dns` (883). Plan estimated ~300 LOC; CoreDNS was rejected for "~20 MB".
- **Experiment (paper, ~0.5 day).** A CoreDNS build with only the needed plugins (`file`/`hosts`,
  `forward`, `cache`) and its actual size; compare against our 883 lines plus tests and the
  ongoing cost of DNS edge cases (EDNS, TCP fallback, truncation).

### I. VIP → keepalived (P2)

- **What we built.** Store-derived VIP allocation, lease-arbitrated holder with ARP announce (843).
  D5.3's argument is real: plain VRRP has no quorum.
- **Candidates.** keepalived with unicast peers and a `track_script` that requires the Raft lease,
  or kube-vip-style leader election.
- **Experiment (paper).** Can keepalived be gated by our lease (holder = lease holder), leaving us
  only the allocator? Compare against `net-vip-failover` timing (≤ 5 s budget).

### J. Firewall → NixOS nftables + set updates (P2)

- **What we built.** Skeleton ruleset created once, set elements updated incrementally through
  `google/nftables`, never a full reload (824).
- **Premise to verify (unverified).** D5.6 says full reloads drop conntrack and break live
  connections. My understanding is that nftables ruleset replacement is atomic and does not flush
  conntrack (that is a separate kernel table). Test it: establish a connection, atomically replace
  the ruleset with `nft -f`, confirm the connection survives. If it does, the "custom incremental
  updater" loses its main justification.

### K. Three liveness mechanisms (P2, internal duplication)

- **What exists.** memberlist gossip (fast failure suspicion), `nodelc` failure monitor (unreachable
  at 15 s, failed at 5 min), and per-node `node-<id>` liveness leases (30 s TTL + skew allowance)
  used by the storage controller to compute `meshedNodes`. Three sources of truth for "is this node
  alive" with different timings.
- **Action.** Write down which decisions depend on which signal; consolidate to one, or make the
  others derived. Adopting B (etcd sessions/leases) would likely collapse two of them.

### L. Linearizability checker → Porcupine (P2, cheap)

- **What we built.** A 246-line Wing & Gong search with memoisation per 4 KiB register. Our release
  criterion ("acked-write loss is a release blocker") leans on it.
- **Action.** Feed the recorded histories through Porcupine (MIT) as a cross-check; if they always
  agree, keep ours or replace it. Half a day. Worth doing regardless of the outcome of A, since any
  future storage layer needs the same checker.

### M–O. Small items (P3)

- **Inventory and health** (0.9 k): `ghw`/`gopsutil`, or reuse node_exporter (Phase 13 ships it).
- **Installer** (0.9 k): already on disko. Check whether the remaining plan/identity/detect glue is
  replaceable with `nixos-anywhere` or `nixos-install` flags.
- **Quantity parser** (0.25 k): apimachinery's `resource.Quantity` or `go-units`; only worth
  changing if a dependency is already present.

## 4. Where we already used existing solutions well

Balance matters: the audit should not read as "everything is wrong". These were adopted
appropriately, and are the model for the rest: hashicorp/raft and memberlist (libraries),
`miekg/dns`, `google/nftables`, `vishvananda/netlink`, `wgctrl`, `go-systemd` D-Bus (instead of
parsing `systemctl`), gRPC/protobuf, cobra, `santhosh-tekuri/jsonschema` (block config), age
(secrets), bbolt, disko + ZFS (layout and storage), and the NixOS impermanence pattern.

## 5. Phases 07–16 (not audited; watch list only)

A keyword pass over the later plans shows mostly *integration* of existing tools (Samba/ctdb,
targetcli, restic, Ollama/vLLM, Prometheus/VictoriaMetrics/Loki, Keycloak, cloud-hypervisor,
Patroni). Traps to check before starting each:

- **P07:** clustered SMB/NFS/iSCSI stand on the volume layer; pick A first or all three inherit it.
- **P09:** VM live migration, if attempted, must lean on the chosen storage layer's semantics.
- **P12:** restic/kopia already exist; keep custom code to the Expanse-specific config backup.
- **P14:** CA rotation and TPM sealing extend F; decide F before building them.
- **P15:** block SDK/registry: prefer OCI/Nix flake distribution over a custom registry.

## 6. Proposed process

**Rubric for each item** (score 0–2 each, dig deeper if total ≥ 5): (1) is it a commodity?
(2) is a mature, actively maintained alternative available? (3) has our own code produced
defects or missed budgets? (4) is it security- or data-critical? (5) does our version depend on
timing/clock assumptions that the alternative has already hardened?

**Suggested spike order** (each time-boxed, each with a pass/fail written *before* starting):

| Order | Spike | Box | Pass criterion |
|---|---|---|---|
| 1 | A: DRBD durability loop, split-brain vs lease, resize/snapshot | 1–2 d | 20 hard-crash iterations with no acked loss; behaviour under partition documented |
| 2 | L: Porcupine cross-check | 0.5 d | Same verdicts on all recorded histories |
| 3 | B: embedded etcd behind `store.Store` | 1–2 d | Conformance green; binary/RSS/latency within budgets |
| 4 | D and E: paper analyses (managers vs NixOS; block model vs Nomad) | 1 d | Written mapping with leftover list |
| 5 | C: HAProxy/Envoy against the LB fixture | 1–2 d | Meets `lb_rps` and added-latency budgets |
| 6 | F: independent security review + step-ca prototype | 1 d | Review findings; enrollment sketch |
| 7 | G–J: short paper analyses (one page each) | 0.5 d each | A recorded requirement or a recorded "keep, because X" |

**Standing rules while the audit is open.**

1. No new features on exvol (T23 perf work, T24 vol-postgres, T25 Ceph stub, T26 protocol doc, T27
   exit checklist) until spike 1 reports. The `vol-perf` harness and budgets are reusable for
   whatever replaces it, so that work is not wasted.
2. Do not start Phases 07+ that consume the volume layer until A is decided.
3. Any new decision record must include: candidates considered, a measured comparison or an explicit
   "not measured", estimated vs. actual LOC when revisited, and licence check.
4. Re-express the footprint budgets (binary 40 MiB, agent RSS 120 MB) as **node-stack** budgets,
   so that adopting an external process (HAProxy, DRBD's kernel module, etcd) is judged on total
   cost, not on the Go binary alone.

## 7. What I have not verified

- Binary-size and RSS impact of embedded etcd, and whether it fits the 40 MiB budget.
- Any performance claim about HAProxy/Envoy relative to our proxy (no measurement).
- DRBD behaviour beyond one perf run, one resync and one crash-promote; no repeated-crash test.
- Nomad's fit to the block model (paper only, not yet done).
- Licences beyond those noted (DRBD is GPL-2.0, Nomad is BSL 1.1, etcd and step-ca Apache-2.0,
  Porcupine MIT — all from memory; confirm before relying on them).
- The D5.6 conntrack claim in either direction.

---

# 8. Second opinion (Opus 5) — revisions to §§2–3

The sections above were written in one pass, reasoning from what each subsystem *is* rather than
from how each subsystem has *behaved*. This section re-examines them against two measurements that
the first pass did not take: the project's own defect history, and which subsystems have
adversarial test coverage. **Three of the first pass's conclusions do not survive.** Where this
section and §§2–3 disagree, this section is the later judgment.

## 8.1 The measurement the first pass skipped: demonstrated defect rate

138 commits, 25 of them fixes. Distribution by subsystem named in the commit subject:

| Subsystem | Fix commits | Adversarial coverage |
|---|---|---|
| **exvol / storage** | **11 of 25 (44%)** | `test/chaos/exvol`, `test/chaos/storage` + linearizability suite, 9 `vol-*` VM tests |
| agent | 2 | 2 VM tests |
| raft / lease / mesh / blocks | 1 each | `test/chaos/cluster` (1675 lines), `test/chaos/blocks` |
| **proxy, DNS, VIP, firewall, scheduler, cluster control** | **0** | `test/chaos/network`, `test/chaos/cluster`, 10 `net-*` + 9 `cluster-*` VM tests |

This matters because the zero-fix areas are **not** untested areas. `test/chaos/cluster` runs random
kill, partition storm, clock skew, slow disk, packet loss and leader churn, plus a checker that is
itself verified against an injected double-hold. `test/chaos/network` runs a VIP partition storm,
WireGuard key rotation and replica-kill-under-load. These subsystems have been shaken and have not
produced fixes.

**Correction to §2's cost model.** The scorecard implicitly priced risk by lines of code. The right
metric is defect density in correctness-critical paths × blast radius. On that metric one subsystem
is an outlier by an order of magnitude and the rest of the tree looks healthy.

**Confound, stated honestly.** exvol is the newest code *and* the most adversarially tested, so some
of its defect count is a maturity artifact rather than a design verdict. Two things stop that from
rescuing it: the perf result (0.16–0.37× local) is architectural, not a bug backlog, and the network
chaos suite (354 lines) is materially thinner than storage's, so "zero fixes" is stronger evidence
for the cluster layer than for the network layer.

## 8.2 Item B (Raft store → embedded etcd): **P0 → P2, and re-framed**

The first pass listed this as co-P0 and described it as "Raft store + leases + watches". That
framing is wrong in a way that matters: **we did not reinvent consensus.** `hashicorp/raft` is used
as intended (`raft.NewRaft`, `raft.Barrier`, `raft-boltdb` for log and stable store). What is ours
is the FSM — roughly 400 lines of `Apply`/CAS/`Txn`/`Snapshot`/`Restore` plus generation retention.
That is the domain state machine any consensus substrate requires; adopting etcd would replace it
with an equivalent mapping onto etcd's own primitives, not delete it.

Further, `store.Store` is already etcd-shaped: revisions, compare-and-swap by revision, watch from a
revision, multi-op `Txn`, literal-prefix `List`. The abstraction is doing its job, which means the
etcd option stays cheaply open. That is an argument for *not* spending the spike now — the cost of
deferring is bounded by an interface we already have and already conformance-test.

**What survives from item B, narrowed.** The `lease` package (526 lines) is wall-clock based with a
skew guard band, and it *has* produced a real defect (a renew loop that stopped permanently on the
first failed CAS, plus a lost-CAS path found in T22). etcd's lease primitive is the thing we would
actually be buying. Recommendation: a targeted review of `internal/cluster/lease` — not a platform
migration. If a future phase needs etcd for other reasons, the interface is ready.

## 8.3 Items C, H, I, J (proxy, DNS, VIP, firewall): **P1/P2 → P3, leave alone**

These were prioritized on the theory that hand-written infrastructure is riskier than adopted
infrastructure. The theory is sound in general and is not supported here: combined, these four
subsystems are ~3.5 k lines, carry adversarial tests, and have produced **zero** fix commits.

The L7 proxy is built on `net/http/httputil.ReverseProxy` — that is already adoption of the standard
library's proxy machinery, not a from-scratch HTTP implementation. Replacing these now would trade
tested, working code for integration risk and an extra process per node, against no measured defect.

Record the rationale and move on. The one item worth keeping from §3-J is the cheap factual check:
**D5.6's premise (that a full nftables reload drops conntrack and breaks live connections) is still
unverified and is probably false** — `nft -f` replaces a ruleset in a single atomic transaction, and
conntrack entries live in a separate kernel table that rule replacement does not flush. This does not
argue for rewriting the firewall code, which works; it argues for correcting the decision record so
the claim is not cited as precedent later.

## 8.4 Item E (scheduler/blocks → Nomad): **strike, do not spike**

Two disqualifiers the first pass treated as footnotes. Nomad moved to the BSL 1.1 in 2023, which is
source-available, not open source — for a product that is itself distributed to users, that is a
licensing decision rather than a technical one, and it should be made deliberately or not at all.
Independently, Nomad carries its own Raft, so adopting it means a second consensus group alongside
ours unless item B is also adopted. The block model is also explicitly a product differentiator per
§1. Recommendation: drop the paper analysis; the scheduler and block engine stay.

## 8.5 Item A (exvol → DRBD): **confirmed, with one risk the first pass underweighted**

Everything in §3-A stands, and the defect data strengthens it: this subsystem accounts for 44% of
all fixes in the project's history, and its perf gap is architectural.

**The underweighted risk: two out-of-tree kernel modules.** We already require ZFS (CDDL,
out-of-tree). Adding DRBD (GPL-2.0, out-of-tree) means *two* out-of-tree modules gating every kernel
upgrade, in a product whose central pillar is deterministic, reproducible upgrades. §3-A listed this
as "maintenance dependency"; it is more than that — it is a direct tension with the determinism
pillar, and it deserves an explicit decision rather than a line item. Mitigating facts: LINBIT
maintains DRBD against current kernels, nixpkgs packages it (9.2.16 builds against the pinned 6.18),
and we have already accepted this class of risk once with ZFS.

The alternative that avoids the second module — pipelining exvol's own write path — remains real but
costs a rewrite of the ordering-critical code that produced those 11 fixes. That is the trade to
decide, and the durability spike is what should decide it.

## 8.6 Revised plan: three spikes, not seven

§6 proposed seven spikes over roughly eight days. Given the defect data, five of them would be
re-litigating decisions the evidence says are fine. Revised:

| Order | Spike | Box | Pass criterion |
|---|---|---|---|
| 1 | **A** — `vol-drbd-durability`: 20-iteration hard-crash loop; split-brain under partition with our lease as promote gate; online resize + snapshot/restore | 1–2 d | 20 iterations, zero acked-write loss; partition behaviour documented |
| 2 | **L** — Porcupine cross-check of recorded histories | 0.5 d | Same verdicts as our checker on every recorded history |
| 3 | **B′** — targeted review of `internal/cluster/lease` (not an etcd migration) | 0.5 d | Renew/expiry/skew paths reviewed against the two known defect classes |

Spike 2 is sequenced second deliberately: it validates the *instrument* we will use to judge spike 1.
Everything else in §2 becomes a recorded decision with its rationale, revisited only if a subsystem
starts producing defects.

**§6's standing rules stand**, with one change: rule 1's hold applies to exvol feature work, but the
`vol-perf` harness, budgets and `vol-drbd-spike` should be committed now — they are the instruments
for the decision, not bets on its outcome.

## 8.7 Net conclusion

The user's premise was correct, and its scope is narrower than §§2–3 implied. We reinvented one
wheel badly (exvol), one wheel defensibly (the lease primitive, worth a review), and otherwise
adopted libraries appropriately — hashicorp/raft, memberlist, miekg/dns, google/nftables, netlink,
wgctrl, go-systemd, gRPC, jsonschema, age, bbolt, disko, `httputil.ReverseProxy`. The first pass
read a single bad outcome as a systemic pattern. The evidence says it is one subsystem, and the
decision in front of us is the storage one.
