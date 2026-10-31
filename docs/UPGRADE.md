# Upgrading

## From the web UI

**Operations → Upgrades** shows each node's version, the newest release and the rollout. While
a node runs an older release than the newest, the Upgrades rail item shows **New**, the footer
links to the release, and the node's row says **Update available**. Choose a release from the
list (the published releases, from the oldest one the cluster runs to the newest) and press
**Upgrade cluster** to move every node to it, one at a time:

1. The leader picks the next node only while the cluster is healthy: a leader, every node
   reachable and in quorum, and every volume Healthy (or UnderReplicated, on a cluster too small
   for its replication). The leader itself goes last, so the rollout causes one election.
2. That node runs `expanse upgrade apply` as its own unit, `expanse-upgrade.service`. It downloads
   the release's source tarball and `SHA256SUMS` from GitHub, and checks the signature on
   `SHA256SUMS` with the release key built into expanse, then the tarball against it. A release that
   ships a binary cache for the node's system is used as a substituter, trusted only with that
   key. The source replaces `/etc/nixos/expanse` (the previous one is kept as `expanse.prev`), the
   system is built from it with the nixpkgs the release pins, set as the boot default and
   switched to. The node records that system as the release's own in
   `/persist/expanse/release-systems.json`, which the node page's Generations tab compares each
   generation with to show drift. A build that fails puts the previous source back. Downloads go to
   `/var/cache/expanse/upgrade/<version>/` and are removed once the node runs the release.
3. Blocks and VMs keep running through the switch. The agent's restart leaves their volumes
   mounted and Primary, and nothing is re-placed or re-elected (`docs/AGENT.md`). A VM whose release
   brings a newer QEMU shows **Update QEMU**, which moves it over without a reboot (`docs/VMS.md`).
4. Once the node runs the release and the cluster is healthy again, the next node starts.

**Pause** stops before the next node; the one upgrading now finishes. A node that fails, or
reports nothing for 45 minutes, **halts** the rollout with its reason. **Retry** tries it again,
and **Cancel** stops the rollout. Nodes already upgraded stay upgraded. If a switch leaves a
node unreachable, the switch watchdog rolls it back and reboots it after 10 minutes.

A node whose kernel changed shows **reboot to finish**. Reboot such nodes one at a time, waiting
for the cluster to be healthy in between.

The leader checks GitHub's list of published releases once a day; **Check now** checks at once, and the
check can be turned off on the same page. `expanse upgrade status` on a node shows its last
upgrade, and `journalctl -u expanse-upgrade` its log. `expanse upgrade start <version>` upgrades
one node by hand.

A cluster can fetch releases from a mirror: set `expanse.agent.releaseURL` (one directory per tag,
holding the release's assets) and, if the mirror re-signs them, `expanse.agent.releaseKey`.

## From 1.11 or earlier (once per node, by hand)

Nodes before 1.12 have no Upgrades page, so the move to 1.12 is done on each node with
`scripts/upgrade-node.sh`. It is also the last upgrade that restarts blocks (the 1.11 agent
empties `/run/expanse` when it stops), so each node is drained first. After it, use the web UI.

The script swaps `/etc/nixos/expanse`, so `configuration.nix` must build from it, as installed
nodes' does (`./expanse/nix/modules/expanse-node.nix`). A node set up by hand whose
`configuration.nix` fetches expanse itself (`expanse = builtins.fetchTarball { url = ...; };`)
needs that binding changed to `expanse = ./expanse;` first; otherwise the script stops, since the
build would still run the old release.

You need root on every node, and the nodes need to reach github.com and cache.nixos.org. Below,
`V` is the release (for example `1.17.1`) and `NODE` is a node's hostname.

**1. Check the cluster and pick the order.** On any node:

```sh
expanse cluster status       # quorum N/majority (a healthy 3-node cluster reads 3/2), every volume UpToDate
expanse ctl node list        # every node healthy, none cordoned
```

Upgrade the nodes one at a time, the leader last (`leader:` in `expanse cluster status` gives its
address; `expanse ctl node list` maps it to a hostname), so the upgrade causes one election.

**2. Drain the node.** Its blocks move to other nodes:

```sh
expanse ctl node drain NODE
expanse ctl block list -n ""   # wait until every block is RUNNING again (on other nodes)
```

On a single-node cluster there is nowhere to move them: run
`expanse ctl node drain NODE --ignore-unplaceable`, and the blocks stop until step 5.

**3. Run the upgrade on the node.** Fetch the script from the release's tag and run it:

```sh
V=1.17.1
curl -fsSLO https://raw.githubusercontent.com/team-expanse/expanse/v$V/scripts/upgrade-node.sh
bash upgrade-node.sh $V
```

It asks whether the node is drained, then logs everything to
`/var/cache/expanse/upgrade/$V/upgrade-node.log`. It stops without changing anything if the
release's signature or checksums do not match, and puts the previous source back if the build
fails.

**4. Reboot if it says so.** When the kernel, initrd or kernel modules changed (moving from 1.11
they do, for the DRBD module), it ends with "reboot this node to finish": `systemctl reboot`, then
wait for the node to come back.

**Or, without draining: `bash upgrade-node.sh --boot $V`.** It builds the release and makes it the
boot default without switching, so the node's blocks keep running until you reboot it; then
skip step 2 and reboot in step 4 when it suits you. The blocks restart once, at the reboot. This
suits a single node, whose blocks cannot move anyway. `--boot` is in the script from 1.12.1 on.

**5. Uncordon it and wait for the cluster:**

```sh
expanse ctl node uncordon NODE
expanse cluster status       # quorum N/majority again, every volume UpToDate, before the next node
```

Repeat steps 2–5 for the next node. Once every node runs `V` (`expanse ctl node list`, or
**Operations → Upgrades**), later releases are upgraded from the web UI. Do not press **Upgrade
cluster** while any node still runs 1.11: it never reports back, and the rollout halts on it after
45 minutes.

### What upgrade-node.sh does

The same steps by hand, if you would rather not run the script:

```sh
V=1.17.1
url=https://github.com/team-expanse/expanse/releases/download/v$V
work=/var/cache/expanse/upgrade/$V
mkdir -p $work && cd $work
for f in SHA256SUMS SHA256SUMS.sig expanse-$V.tar.gz expanse-$V-cache-x86_64-linux.tar; do
  curl -fsSLO $url/$f
done

# The release key (internal/upgrade/release-key.pub) as openssl reads it.
cat >release-key.pem <<'EOF'
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAAnEv4AWuSplcqvtjB9OmACQbT6qfPSv82MjwzBSaLFM=
-----END PUBLIC KEY-----
EOF
base64 -d SHA256SUMS.sig >SHA256SUMS.sig.bin
nix-shell -p openssl --run 'openssl pkeyutl -verify -pubin -inkey release-key.pem -rawin -in SHA256SUMS -sigfile SHA256SUMS.sig.bin'
grep -v '\.iso$' SHA256SUMS | sha256sum -c   # both must pass; stop here if either fails

mkdir -p cache && tar -xf expanse-$V-cache-x86_64-linux.tar -C cache
cd /etc/nixos
tar -xzf $work/expanse-$V.tar.gz
rm -rf expanse.prev; [ -d expanse ] && mv expanse expanse.prev
mv expanse-$V expanse
sed -i 's|/etc/expanse/flake/|./expanse/|g' configuration.nix   # only matters on 1.10.0 nodes
sys=$(nix-build expanse/nix/node-system.nix -A config.system.build.toplevel --no-out-link \
  --option extra-substituters file://$work/cache \
  --option extra-trusted-public-keys expanse-release-1:AnEv4AWuSplcqvtjB9OmACQbT6qfPSv82MjwzBSaLFM=)
nix-env -p /nix/var/nix/profiles/system --set "$sys"
"$sys"/bin/switch-to-configuration switch     # or: boot, and reboot later (--boot)
```

The build substitutes everything: from cache.nixos.org, and from the release's own cache for what
cache.nixos.org lacks (expanse itself, the headless qemu). It compiles nothing heavy.

**Rolling back** a node: `cd /etc/nixos && rm -rf expanse && mv expanse.prev expanse`, then
`nixos-rebuild switch`, or pick the previous generation in the boot menu.

## By hand (rolling upgrade)

How to move a live cluster from one `expanse` build to another, one node
at a time, without losing quorum, without losing an acked write, and
without a read/write gap the cluster as a whole should ever show a
client. Written from what `nix/tests/cluster-rolling-upgrade.nix`
actually proved (Phase 11, X3), not speculated in advance — see
`ARCHITECTURE.md` A44 for the full account and the bugs it found.

### Mechanism

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

### Before switching a node that holds a volume Primary

**Since 1.12 no drain is needed.** Block units are not restarted by a switch, and an agent that
stops while a workload still has a volume open leaves it Primary for the next agent, which takes
it back without unmounting it (`nix/tests/node-upgrade-keeps-blocks.nix`). The rest of this
section describes nodes before 1.12. The upgrade *to* 1.12 is the last one that restarts blocks:
the 1.11 agent still empties `/run/expanse` when it stops. Nodes before 1.12 also have no rollout,
so that upgrade is done by hand on each node ("From 1.11 or earlier", above).

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

### Sequencing

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

### What continuous availability actually looks like mid-switch

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

### Version compatibility

`proto/store.proto` and `raftstore.CommandVersion` (the replicated log's
wire encoding) have not changed since Phase 3 — confirmed by this
stream's own git history check before picking which two builds to test
against. A rolling upgrade across two builds that do not change
`CommandVersion` is what this document and its VM test actually prove
safe: old- and new-version nodes interoperate over gRPC/raft for the
whole upgrade window, old data survives being read and re-verified under
new-version software, and the switch mechanism itself works end to end.

**Two-replica tiebreakers (1.2.4).** A tiebreaker is recorded beside a volume's replicas, so
agents older than 1.2.4 ignore it: an old agent on the tiebreaker's node does not run it, and an
old agent holding a replica keeps quorum off. Nothing breaks; the volume simply has no partition
protection until every node it touches runs 1.2.4 or later. Upgrade all nodes before relying on it.

**`decodeCommand` rejects an unrecognized `CommandVersion` outright** — a
v1 reader does not gracefully skip a v2 entry, it errors. A future change
that bumps `CommandVersion` is *not* covered by this proof and needs its
own dual-version transition plan (e.g. a reader that accepts both
versions for one release before writers are allowed to emit the new one)
before a rolling upgrade across that boundary can be trusted the way an
ordinary code-only upgrade already is.

### What the VM test proves vs. production

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
