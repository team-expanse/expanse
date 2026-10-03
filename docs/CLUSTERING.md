# Clustering

How Expanse turns independent agents into one strongly consistent,
mutually authenticated cluster: Raft-replicated state over mTLS,
memberlist gossip for metadata, single-writer leases as the
anti-split-brain primitive, generations for fleet-wide configuration,
and cordoned/drain/remove interlocks for node lifecycle.

Implementation map:

| Concern | Package | Notes |
|---|---|---|
| Replicated KV + leases + generations FSM | `internal/store/raftstore` | hashicorp/raft over mTLS |
| CA, certs, TLS config | `internal/cluster/ca` | one cluster CA; per-node client+server certs |
| Join protocol, tokens | `internal/cluster/join` | single-use, TTL-bounded tokens; :7446 |
| Enrollment CLI flow | `internal/cluster/control` | `expanse cluster init/join/status/token` |
| Membership gossip | `internal/cluster/membership` | memberlist over mTLS; :7445 |
| Leases | `internal/cluster/lease` | CAS through the raft store; guard band |
| Generations | `internal/cluster/generation` | monotonic fleet config; rollback |
| Node lifecycle | `internal/cluster/nodelc` | cordon / drain / remove |
| Witness | `internal/agent` role `witness` | full voter, never placeable |

Ports: 7443 API + internal gRPC (mTLS), 7444 raft, 7445 memberlist,
7446 join.

## Raft layer

- **Membership**: the raft configuration is itself raft-replicated.
  `cluster init` bootstraps node 1 as the sole voter; joiners are added
  with `AddVoter` from the leader, after the join service has verified a
  single-use join token and the joiner's client certificate chains to
  the cluster CA.
- **Writes**: `Put`/`Txn` propose a raft entry on the leader; followers
  and fresh CLIs forward to the leader (`internal/store/raftstore/forward`)
  with a short retry deadline (~2 s), so a write against a deposed leader
  fails fast instead of hanging.
- **Reads**: linearizable by default (one `Barrier` round trip on the
  leader — quorum-acknowledged). Stale reads (`store.WithStale`) serve
  from the local FSM with no round trip and are opt-in only.
- **Degraded mode**: with no leader reachable, a node marks itself
  read-only (degraded): linearizable reads/writes fail
  `unavailable`; reconcile freezes; stale reads keep local insight.
- **Snapshots**: bolt-backed log store + FSM snapshots; a restarted or
  rejoining node catches up via log replay or `InstallSnapshot`.

## Administering a running cluster

These commands go through the local agent, so run them from any node
with every agent up. Writes and raft membership changes are forwarded
to the leader.

| Command | Does |
|---|---|
| `expanse cluster token create/list/revoke` | Manage join tokens |
| `expanse cluster ca rotate/status/complete` | Rotate the cluster CA (`docs/SECURITY.md` §2) |
| `expanse ctl node list` | Nodes with role, lifecycle, cordon and last-seen |
| `expanse ctl node cordon <id>` | Stop new placements on a node; its replicas keep running |
| `expanse ctl node drain <id>` | Cordon a node and move its replicas, daemonsets included, elsewhere |
| `expanse ctl node uncordon <id>` | Resume placements on a node and end any drain |
| `expanse ctl node transfer-leadership [id]` | Move raft leadership (default: the most up-to-date follower) |
| `expanse ctl node remove <id>` | Remove a node from raft and revoke its identity |
| `expanse cluster leave [id]` | Same as `node remove`, defaulting to this node |

Removing the leader is refused: transfer leadership first. Removal
writes the revocation before changing raft membership, and the leader
finishes any removal its caller could not, such as a node removing
itself. The `cluster` commands above also work with the agent stopped (right
after `cluster init`): they then open the node's store directly.

## Join security

`expanse cluster token create` mints a single-use, TTL-bounded,
HashHMAC-SHA256 token (checked into the raft store at join time, so
consumption is replicated and race-safe: exactly one of two simultaneous
joins wins; the loser sees `conflict`). The joiner presents a
self-generated ed25519 key; the leader issues a certificate only after
verifying the token and the joiner's TLS proof of possession of the
private key. All join traffic is mTLS end-to-end (verified in
`nix/tests/cluster-join-security.nix`: invalid/expired/consumed tokens
rejected, zero cleartext on the wire, self-signed client certs rejected
with `unknown ca`).

## Lease safety argument

A lease is a store key `/leases/<name>` whose JSON value names the
holder, the granting term, and an expiry on the **granting leader's
wall clock**. Possession is a compare-and-swap through the replicated
store, so every grant/takeover/renewal goes through one raft log and is
observed in one order by all nodes.

Constants (`internal/cluster/lease`): `TTL = 15 s` (default),
renewal cadence `TTL/3 = 5 s`, renewal CAS budget `TTL/3`, takeover
permitted only when `now > ExpiresAt`.

The argument (spec §4.3, verbatim):

> A lease granted at time T expires at T+TTL by the *leader's* clock.
> The holder renews at T+TTL/3. If the holder is partitioned, its
> renewal fails, and it closes `Done()` at latest by T+TTL/3+renewal_timeout.
> The leader will not grant the lease to another node until T+TTL.
> Since TTL/3 + renewal_timeout (5s+2s=7s) < TTL (15s), there is a
> ≥8 s guard band during which the old holder has stopped and the new
> holder has not started. This holds as long as clock *rates* differ
> by less than ~50%, which NTP-synced machines always satisfy. It does
> not depend on clock *offset* agreement.

Concrete walkthrough:

1. **Happy path.** The holder renews every `TTL/3`; each renewal is a
   CAS that advances the stored expiry. Its `Held.Valid()` checks the
   *local* monotonic axis (`time.Since` anchors and the renewal ticker
   are monotonic), never a wall clock.
2. **Holder partitioned.** Renewal RPCs fail (write forwarding needs
   quorum). After at most one failed renewal cycle the holder closes
   `Done()` — at latest `TTL/3 + renewal budget ≈ TTL/3 + TTL/3 < TTL`.
   The old holder *must have stopped* acting by then.
3. **Takeover.** A would-be taker sees `now > ExpiresAt` (its local
   clock vs the granting node's persisted expiry — the one wall-clock
   comparison the model tolerates) and CASes the lease record. Because
   the takeover happens no earlier than `T+TTL` while the old holder
   stopped by `≈ T+2TTL/3`, there is a **≥ 8 s guard band** in which
   neither side acts. Two live contenders can never both believe
   themselves holders: whichever CAS lands second sees the renewed
   record and gets `ErrNotAcquired`.
4. **Clock discipline (spec §10).** All *local timing* — renewal
   cadence, slow-renewal detection, the `Done()` stop deadline — is
   monotonic (`time.Ticker`, context deadlines, `time.Since` anchors).
   Wall clock appears in exactly two places, both offset-bounded: the
   persisted `ExpiresAt` (written once by the granting node) and the
   takeover test. A grantor up to `~TTL/2` ahead of a taker stays safe;
   NTP keeps real offsets in milliseconds against an ≥ 8 s band. A
   suspended VM resuming with a wall-clock jump cannot extend a lease —
   renewal timers and `Done()` live on the monotonic axis.
5. **Rate drift.** The argument only needs clock *rates* within 50%,
   which NTP guarantees; offset agreement is never required.
6. **Verification.** `internal/cluster/lease` tests cover the guard
   band, takeover races, renewal failure and expiry. The chaos suite
   (`test/chaos/cluster`) runs the clock-skew scenario (±10 s offsets +
   1.05× rate drift, 200 takeovers over 5 min) and partition-storm with
   lease churn — the split-brain invariant checker polls every node's
   FSM at 100 ms and asserts *at most one believing holder* across all
   of it; a second checker binary runs as a separate process against
   the live node APIs (a double-hold makes it exit 1 — verified by a
   negative test that injects one).

## Split-brain invariant checker

Per spec §6, the single most important invariant runs *outside* the
cluster, as a separate process (`test/chaos/cluster/checker`):

```
every 100ms:
  for each lease L in the cluster:
     holders = [n for n in nodes if n believes it holds L]
     assert len(holders) <= 1, "SPLIT BRAIN: {holders} both hold {L}"
```

"Believes it holds" means: the node's lease record carries the **newest
revision seen anywhere** and is **unexpired**. A stale, lower-revision
copy on a partitioned node is not a holder — the majority's takeover of
an expired lease is legitimate and must not false-positive. Two
disagreeing FSMs at the same newest revision *is* a split brain and is
reported immediately. In CI the checker runs in-process at 100 ms for
every chaos scenario; `make chaos` (nightly) runs all six scenarios at
the full five minutes each.

## Failure matrix

| Fault | Detection | Behavior | Recovery |
|---|---|---|---|
| Leader process dies | followers' heartbeat timeout (~1–1.5 s) | new election among voters; writes/linearizable reads fail `unavailable` meanwhile | new leader serves immediately; old leader rejoins as follower |
| Follower dies | gossip `alive=false` + raft log lag | quorum (2/3) keeps serving; reconcile continues | restart → log replay or snapshot catch-up |
| Quorum lost (2 of 3 down) | remaining node: no leader | survivor degrades: read-only, writes rejected `unavailable`, reconcile frozen | any node restored → quorum → leader elected, unfreeze |
| Network partition: minority isolated | raft RPC timeouts on both sides | minority: degraded read-only; majority: re-elects, keeps serving; old leader steps down (lease check) | partition heals; minority syncs; zero divergence (VM-tested) |
| Partition: leader alone | leader's `checkLeaderLease` fails; followers time out | leader steps down before serving stale writes; minority elects a new leader | same as above |
| Clock skew (offset) | none (by design — tolerated) | lease safety holds for offsets ≪ guard band; monotonic local timing unaffected | NTP resync |
| Clock rate drift | none (tolerated while < 50%) | guard-band argument intact | NTP discipline |
| Slow disk (fsync latency) | raft writes slow; heartbeats may miss | election may flap; writes still commit, just slower (chaos-tested at 500 ms fsync) | latency drops → stability returns |
| Packet loss 5–50% | dial/replication errors | leader may flap; acked writes are never lost (chaos-tested) | loss clears → converge |
| Witness node dies | gossip | 2 voters + witness: quorum needs 2 voters — service continues | witness restarts, re-joins raft |
| Voter node removed (`cluster node remove`) | explicit | leader transfers leadership off the target; node removed from raft config; memberlist leaves | fleet keeps quorum |
| Join with reused/expired token | join service checks replicated token state | join rejected `invalid argument`/`conflict`; exactly one concurrent join wins | operator mints a new token |
| CA-compromised client cert | cert must chain to cluster CA | TLS handshake fails (`unknown ca`); nothing joins | rotate CA (out of scope here) |
| Suspended VM, clock jump | n/a | lease cannot be extended; monotonic deadlines unaffected | VM resumes, renews |

## Operational notes

- **Formation timing**: 3/5/9-node formation is 1–2 s in the perf suite
  (`test/perf`, budget 30 s).
- **Latency budgets** (`test/perf/budgets.yaml`, all ≥ 1000× headroom in
  practice): 1000 sequential writes p99 ≤ 200 ms (measured ≈ 0.3 ms);
  10k linearizable reads p99 ≤ 50 ms (≈ 0.3 ms); stale reads p99 ≤ 5 ms;
  leader election p99 ≤ 5 s (≈ 2.5 s); snapshot/restore of 100k keys.
- **Witness**: a full raft voter with `placeable: false` — it breaks
  ties in a 2+1 topology without ever running workloads.
- **Generations** are FSM-side state with retention (last 50 / 30 days),
  hash-addressed, rollback-checked (VM-tested: rollback hash equality).
- **Single-node clusters**: `expanse cluster init ... --expect 1` forms a
  working cluster on one machine (quorum `1/1`). Volumes and blocks run
  there with no redundancy (`docs/STORAGE.md` §8) and grow to their
  replication target as nodes join with `cluster token create` /
  `cluster join`. One node gives no tolerance to node loss; raft needs
  three voters (or two plus a witness) to survive one failure.
