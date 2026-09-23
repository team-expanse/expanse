"""PHASE-04-TASKS.md Stream B: deploy a SINGLETON iscsi/target block on a
raw (no-filesystem) DRBD-backed volume; an external open-iscsi initiator
discovers the portal at the block's VIP, logs in, and a write through the
LUN round-trips correctly and checksums equal from a second, independent
initiator-side read AND from the primary node's own direct view of the
raw device (X1) — the same "two readers of one truth" discipline Phase 2's
E1 and Phase 3's D1 established. X2/X4 (kill-mid-write failover, PR
survival) are Stream C's own test, not this one.

Runs after cluster-common.py, client-common.py (with `client` bound to
the external VM), block-common.py and vol_cluster.py. Expects VIP_POOL
(two addresses, iscsi-target.nix) spliced in ahead of this file.
"""

PORT = 3260  # the client-facing, VIP-exposed portal port
# LIO's own internal listen port must differ from PORT, matching
# defaults.yaml — the VIP holder's own listen check reserves PORT on
# whichever node it moves to, which collides with LIO already bound to
# 0.0.0.0:PORT on the very node the VIP is trying to move TO (the same
# reason share/smb keeps its own internal port apart from 445).
LIO_PORT = 33260
IQN = "iqn.2026-09.io.expanse:test-lun"
INITIATOR_IQN = "iqn.2020-08.org.linux-iscsi.initiator:test"
LUN_MIB = 32

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: lun
  namespace: default
spec:
  type: iscsi/target
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
  storage:
    - name: lun
      size: {LUN_MIB}Mi
      replication: 3
      mountPath: /mnt/lun
      filesystem: none
  config:
    port: {LIO_PORT}
    iqn: {IQN}
  network:
    ports:
      - name: iscsi
        port: {PORT}
        target_port: {LIO_PORT}
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: {LIO_PORT}
        period_seconds: 2
"""


def vip_holders(vip_addr):
    """Nodes currently carrying vip_addr on eth1."""
    holders = []
    for m in [n1, n2, n3]:
        rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {vip_addr} || true")
        if rc == 0 and out.strip():
            holders.append(m.name)
    return holders


def new_block_device(before, timeout=60):
    """The one block device name that appears under client's /dev after
    before (a set of pre-login `lsblk -ndo NAME` names) was captured."""
    deadline = time.time() + timeout
    now = set()
    while time.time() < deadline:
        now = set(client.succeed("lsblk -ndo NAME").split())
        new = now - before
        if len(new) == 1:
            return "/dev/" + next(iter(new))
        time.sleep(1)
    raise AssertionError(f"no single new block device after iscsi login: before={before} now={now}")


form("iscsitgt")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)

with subtest("the cluster's own management UI claims one pool address"):
    # Same two-address dance share_smb.py uses: the UI's own VIP
    # (internal/agent/ui_vip.go) claims one pool address unconditionally,
    # independent of any block, and the block's own VIP request must
    # find the other one free.
    deadline = time.time() + 60
    ui_vip = None
    while time.time() < deadline and ui_vip is None:
        for pool_addr in VIP_POOL:
            if len(vip_holders(pool_addr)) == 1:
                ui_vip = pool_addr
                break
        if ui_vip is None:
            time.sleep(2)
    assert ui_vip, f"no VIP claimed within 60 s (expected the UI to take one): {VIP_POOL}"

with subtest("deploy a SINGLETON iscsi/target block bound to a raw volume"):
    deploy(n1, "lun", MANIFEST)
    b = wait_phase(n1, "lun", ["RUNNING"], 180)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"target placed on {nodes}, want exactly 1: {b.get('status')}"

with subtest("the target places on (and only on) the DRBD primary"):
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    # A fresh volume's very first primary election is arbitrary (whichever
    # node notices first, independent of where anything is scheduled);
    # the block's own placement then drives the storage controller to
    # move DRBD's primary to match it (P12/D2's colocation guarantee is
    # an eventually-converged property, not synchronous with the block
    # reaching RUNNING) -- wait for that convergence instead of asserting
    # it on the first sample, the same discipline daemonset_raw_colocation
    # .py uses for its own primary-election wait.
    wait_for(lambda: {m.name for m in primaries(res)} == nodes,
             "DRBD primary to converge on the target's own placement", 90)
    prim = primaries(res)[0]

with subtest("the block claims the other pool address as its own VIP"):
    remaining = [a for a in VIP_POOL if a != ui_vip]
    assert len(remaining) == 1, f"VIP_POOL must have exactly 2 addresses: {VIP_POOL}"
    VIP = remaining[0]
    deadline = time.time() + 60
    holder = None
    while time.time() < deadline:
        hs = vip_holders(VIP)
        if len(hs) == 1 and hs[0] in nodes:
            holder = hs[0]
            break
        time.sleep(2)
    assert holder, f"target's VIP ({VIP}) never settled on its own placement {nodes} (last holders: {vip_holders(VIP)})"

with subtest("an external initiator logs in to the portal at the block's VIP"):
    # SendTargets discovery is deliberately not used here: its response
    # carries LIO's OWN view of its portal address (0.0.0.0:LIO_PORT, or
    # whatever local address the splice's loopback connection resolves
    # to) -- not the external VIP:PORT the initiator actually dialed, since
    # LIO has no notion of the vip.Holder splice sitting in front of it.
    # Confirmed directly: the SendTargets query itself succeeds through
    # the VIP, but the node record it produces doesn't match VIP:PORT, so
    # a --login against that record fails with "No records found". Static
    # node registration (-o new) sidesteps this entirely by pointing the
    # initiator at VIP:PORT directly, never trusting what the target
    # reports about its own address -- the supported recipe this phase
    # documents (Stream D's docs/ISCSI.md), not a test-only workaround.
    before = set(client.succeed("lsblk -ndo NAME").split())
    client.succeed(f"iscsiadm -m node -o new -T {IQN} -p {VIP}:{PORT}")
    client.succeed(f"iscsiadm -m node -T {IQN} -p {VIP}:{PORT} --login")
    dev = new_block_device(before)

with subtest("a write through the LUN round-trips and checksums equal on a second read"):
    client.succeed(f"dd if=/dev/urandom of={dev} bs=1M count=1 oflag=direct conv=fsync")
    ref = checksum(client, dev, 1)
    client.succeed(f"iscsiadm -m node -T {IQN} -p {VIP}:{PORT} --logout")
    client.wait_until_succeeds(
        f"iscsiadm -m node -T {IQN} -p {VIP}:{PORT} --login", timeout=30
    )
    got = checksum(client, dev, 1)
    assert got == ref, f"checksum mismatch on second initiator-side read: {got} != {ref}"

with subtest("the write is visible from the primary's own view of the raw device"):
    # Two independent readers of the same underlying bytes (the initiator
    # over iSCSI, and the primary node reading its local DRBD device
    # directly) — the same discipline Phase 2's E1 and Phase 3's D1 used,
    # applied here to block I/O instead of a file or a share.
    host_got = checksum(prim, device_of(prim), 1)
    assert host_got == ref, f"checksum mismatch on the primary's own device view: {host_got} != {ref}"

print("ISCSI-TARGET DONE")
