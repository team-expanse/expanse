# Rolling Upgrade

How to move a live cluster from one `expanse` build to another, one node
at a time, without losing quorum, without losing an acked write, and
without a read/write gap the cluster as a whole should ever show a
client. Written from what `nix/tests/cluster-rolling-upgrade.nix`
actually proved (Phase 11, X3), not speculated in advance — see
`ARCHITECTURE.md` A44 for the full account and the bugs it found.

## Mechanism

An upgrade is an ordinary `nix-config` resource apply (`docs/AGENT.md`'s
"Nix switches and the watchdog"): the agent builds a flake attribute with
`nix build`, then runs `switch-to-configuration switch` on the resulting
system closure — the same activation script `nixos-rebuild switch` itself
runs, restarting whichever systemd units differ (`expansed.service` among
them when the `expanse` package changed) and installing a new boot entry.
There is no cluster-level orchestrator that walks the fleet for you; apply
the resource to one node, confirm it healthy, then the next:

```console
$ expanse ctl resource apply - <<'EOF' --socket /run/expanse/agent.sock
nix-config:node:
  type: nix-config
  flake: path:/etc/nixos
  attr: nixosConfigurations.expanse-node.config.system.build.toplevel
  switch_mode: switch
EOF
```

run against one node's own socket at a time (`expanse ctl resource apply`
targets whichever node's socket you give it — there is no fleet-wide
apply). The pending-switch watchdog (`expanse-switch-watchdog.timer` →
`expanse watchdog`) is the safety net if the agent dies mid-switch: a
marker stale past 10 minutes triggers an automatic rollback to the
previous generation and a reboot, so a bad remote config change can never
brick a node.

## Before switching a node that holds a volume Primary

**Drain it first.** Switching a node's `expanse` build restarts
`expansed.service`, whose `ExecStopPost` demotes every DRBD volume it
holds Primary — but a process that still has the device open (a running
block workload, or any raw writer) blocks that demote indefinitely: the
kernel refuses `drbdsetup secondary` with "device held open by someone,"
and both the stop hook and the restarted agent's own step-down retry it
forever, logged repeatedly as `leading interrupted`. This is not a
timing fluke; it reproduced deterministically the first time this stream
ran the test with continuous load. Run `expanse ctl node drain <id>`
before switching it: the node's block replicas, daemonsets included, move
to other nodes within a few controller periods. Wait until
`expanse ctl block get` shows them running elsewhere, and stop anything
else that holds the volume open on that node (a raw writer) yourself.
Plain `cordon` is not enough; it only stops new placements.

Once demotion is unblocked, do not assume Primary moves to a different
node: unlike a hard node loss (`vol-durability.nix`, `chaos-soak.nix`),
a graceful switch never actually removes the node from the mesh — it is
back and healthy within seconds — so the placement controller commonly
re-promotes the *same* node rather than handing off to a survivor. Either
outcome is correct; wait for "exactly one Primary somewhere among all
nodes," not "a survivor became Primary."

## Sequencing

For each node, one at a time:

1. Confirm the cluster is fully healthy first: `expanse cluster status`
   reports quorum `N/majority` (a healthy 3-node cluster reads `3/2` —
   that is the fully-healthy string, not `3/3`) and every volume shows
   `UpToDate` on every replica.
2. If this node holds a volume Primary, drain it (above).
3. Apply the `nix-config` resource for this node (switch_mode `switch`).
4. Wait for `expansed.service` active and the agent socket back up.
5. Wait for quorum to read `N/majority` again and every volume to show
   `UpToDate` on every replica again before touching the next node.
6. If you drained it, `expanse ctl node uncordon <id>` so it takes
   placements again.

Skipping step 5 (moving to the next node before the current one has
fully rejoined) is the one thing that can cost the cluster a majority:
with 3 nodes and quorum 2, a second node down before the first is back
loses quorum outright.

## What continuous availability actually looks like mid-switch

A surviving node's own KV reads/writes keep working throughout another
node's switch — proven by a background put loop against each node's own
local socket during the whole rolling upgrade, not just spot-checked
before and after. The one caveat found live: a single, momentary blip on
a *surviving* node's writes is normal while raft notices the switched
peer bouncing and recomputes who it needs for quorum — ordinary
member-churn behavior, not an outage, and not sustained. What actually
matters is that writes resume immediately and no node goes dark for more
than that one moment; a real multi-second-or-longer stall on a surviving
node would be a genuine regression, not this.

## Version compatibility

`proto/store.proto` and `raftstore.CommandVersion` (the replicated log's
wire encoding) have not changed since Phase 3 — confirmed by this
stream's own git history check before picking which two builds to test
against. A rolling upgrade across two builds that do not change
`CommandVersion` is what this document and its VM test actually prove
safe: old- and new-version nodes interoperate over gRPC/raft for the
whole upgrade window, old data survives being read and re-verified under
new-version software, and the switch mechanism itself works end to end.

**`decodeCommand` rejects an unrecognized `CommandVersion` outright** — a
v1 reader does not gracefully skip a v2 entry, it errors. A future change
that bumps `CommandVersion` is *not* covered by this proof and needs its
own dual-version transition plan (e.g. a reader that accepts both
versions for one release before writers are allowed to emit the new one)
before a rolling upgrade across that boundary can be trusted the way an
ordinary code-only upgrade already is.

## What the VM test proves vs. production

`nix/tests/cluster-rolling-upgrade.nix` switches each node via its own
pre-built `specialisation.upgraded` and `switch-to-configuration test`
(activate, skip the boot-loader entry) rather than `switch-to-configuration
switch` through a live `nix-config` resource apply: the test's VM nodes
have no real EFI/bootloader partition for `switch` mode's boot-entry step
to write to (found live — the first run failed there), and a `nix build`
inside the guest would need real network/eval plumbing this harness does
not set up. Both are test-infrastructure simplifications, not mechanism
differences: `test` mode runs the identical activation script that
restarts changed services, which is the actual mixed-version-interop
question this stream exists to answer; a real deployment additionally
exercises the `nix build` and boot-entry steps, already covered
separately by every other install/switch path in this project.
