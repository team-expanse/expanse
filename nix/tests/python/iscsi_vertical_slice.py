"""PHASE-04-TASKS.md Stream D (D1): the phase-closing vertical slice.
One combined test proving X1, X2, X4 and X5 together, unlike Phase 3's
per-protocol split -- there is only one protocol here.

Unlike Stream C's iscsi_target_failover.py, which explicitly waits on
internal state (replica UpToDate, DRBD primary convergence, VIP holder)
as separate pre-kill subtests, this test stays black-box: it deploys,
then simply retries the initiator's own login against the target's VIP
until it works, and kills whichever node the VIP is actually on the
instant the write loop is confirmed flowing -- "kill mid-deploy, not
after an assumed placement" (PHASE-04-TASKS.md Stream D). The post-kill
re-convergence wait is unavoidable (a survivor must be identified to
read its device directly), but nothing before the kill is assumed.

Runs after cluster-common.py, client-common.py (with `client` bound to
the external VM), block-common.py, vol_cluster.py and iscsi_common.py.
Expects VIP_POOL (two addresses, iscsi-vertical-slice.nix) spliced in
ahead of this file.
"""

PORT = 3260
LIO_PORT = 33260  # collision note: iscsi_target.py
IQN = "iqn.2026-09.io.expanse:test-lun-vslice"
INITIATOR_IQN = "iqn.2020-08.org.linux-iscsi.initiator:vslice"
LUN_MIB = 64
MOUNT_PATH = "/mnt/lun"
MIN_ACKS_BEFORE_KILL = 5
MIN_ACKS_AFTER_RECOVERY = 5
PR_KEY = "0xdef456"

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
      mountPath: {MOUNT_PATH}
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

ALL_MACHINES = {"n1": n1, "n2": n2, "n3": n3}

form("iscsivs")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)

with subtest("the cluster's own management UI claims one pool address"):
    deadline = time.time() + 60
    ui_vip = None
    while time.time() < deadline and ui_vip is None:
        for pool_addr in VIP_POOL:
            if len(vip_holders(pool_addr, NODES)) == 1:
                ui_vip = pool_addr
                break
        if ui_vip is None:
            time.sleep(2)
    assert ui_vip, f"no VIP claimed within 60 s: {VIP_POOL}"
    remaining = [a for a in VIP_POOL if a != ui_vip]
    assert len(remaining) == 1, f"VIP_POOL must have exactly 2 addresses: {VIP_POOL}"
    VIP = remaining[0]

with subtest("deploy the target and log in the moment its portal is reachable (X1)"):
    # No internal placement/DRBD/VIP-settle checks here on purpose: the
    # login retry loop itself is the only proof of readiness this test
    # relies on, mirroring how a real initiator would connect.
    deploy(n1, "lun", MANIFEST)
    client.succeed(f"iscsiadm -m node -o new -T {IQN} -p {VIP}:{PORT}")
    before = set(client.succeed("lsblk -ndo NAME").split())
    client.wait_until_succeeds(
        f"iscsiadm -m node -T {IQN} -p {VIP}:{PORT} --login", timeout=180
    )
    dev = new_block_device(before)

with subtest("a continuous write loop starts and is confirmed flowing"):
    start_writer(dev)
    wait_acked_at_least(MIN_ACKS_BEFORE_KILL, 60, "pre-kill warmup")

with subtest("register a SCSI-3 persistent reservation before the kill (X4 setup)"):
    client.succeed(f"sg_persist --out --register --param-sark={PR_KEY} {dev}")
    client.succeed(f"sg_persist --out --reserve --param-rk={PR_KEY} --prout-type=1 {dev}")

with subtest("kill whichever node the VIP is actually on, the instant I/O is flowing"):
    holders = vip_holders(VIP, NODES)
    assert len(holders) == 1, f"expected exactly one VIP holder at kill time, got {holders}"
    holder = holders[0]
    acked_at_kill = last_acked()
    t0 = time.time()
    ALL_MACHINES[holder].crash()

with subtest("the block, its volume primary and its VIP all re-converge on one survivor"):
    survivors = {n: m for n, m in ALL_MACHINES.items() if n != holder}
    survivor = next(iter(survivors.values()))
    res = survivor.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    deadline = time.time() + 240
    new_holder = None
    last_seen = {}
    while time.time() < deadline:
        b = get_json(survivor, "lun")
        cur_nodes = placement_nodes(b) if b else set()
        if len(cur_nodes) == 1 and holder not in cur_nodes:
            candidate = next(iter(cur_nodes))
            candidate_m = ALL_MACHINES[candidate]
            primary_role = role_of(candidate_m, res)
            vips = vip_holders(VIP, list(survivors.values()))
            last_seen = {"placement": candidate, "primary_role": primary_role, "vip_holders": vips}
            if primary_role == "Primary" and vips == [candidate]:
                new_holder = candidate
                break
        else:
            last_seen = {"placement": sorted(cur_nodes)}
        time.sleep(2)
    assert new_holder, \
        f"target, its volume primary and its VIP never agreed on one survivor within 240s: {last_seen}"
    new_holder_m = ALL_MACHINES[new_holder]
    reconverge_s = time.time() - t0
    print(f"target, primary and VIP all agree on {new_holder} after {reconverge_s:.1f}s")

with subtest("the initiator's writer resumes without a manual re-login (X2)"):
    resumed_at = wait_acked_at_least(acked_at_kill + MIN_ACKS_AFTER_RECOVERY, 180,
                                      "post-failover writes to resume")
    stall_s = time.time() - t0
    print(f"writer resumed: {resumed_at} acked (was {acked_at_kill} at kill); stall {stall_s:.1f}s")

with subtest("stop the writer and every acked record checksums correctly, two ways (X2)"):
    client.succeed("systemctl stop iscsi-writer 2>/dev/null || true")
    final_acked = last_acked()
    verify_records(client, dev, final_acked, "initiator's own read-back")
    survivor_dev = device_of(new_holder_m)
    verify_records(new_holder_m, survivor_dev, final_acked, f"survivor {new_holder}'s own device")

with subtest("the raw storage entry was never formatted or mounted (X5)"):
    # General plumbing already proven by Stream A/B1 (Exit criteria
    # table); re-checked here as part of the same combined slice rather
    # than assumed to still hold after a real failover.
    fstype = new_holder_m.succeed(f"blkid -o value -s TYPE {survivor_dev} 2>/dev/null || true").strip()
    assert fstype == "", f"raw LUN device {survivor_dev} was formatted: TYPE={fstype!r}"
    for m in survivors.values():
        mounts = m.succeed(f"grep -F {MOUNT_PATH} /proc/mounts || true").strip()
        assert mounts == "", f"{m.name}: {MOUNT_PATH} unexpectedly mounted: {mounts!r}"

with subtest("X4: does the persistent reservation survive failover?"):
    # Not a release gate (Exit criteria, D4) -- reported, not asserted,
    # consistent with Stream C's own measured "did not survive" result.
    keys_after = client.execute(f"sg_persist --in --read-keys {dev}")[1]
    survived = PR_KEY[2:] in keys_after.lower()
    verb = "SURVIVED" if survived else "DID NOT survive"
    print(f"X4: PR key {PR_KEY} {verb} failover -- keys now: {keys_after!r}")

print("ISCSI-VERTICAL-SLICE DONE")
