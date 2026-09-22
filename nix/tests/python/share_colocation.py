"""PHASE-03-TASKS.md Stream A1: a SINGLETON block bound to a volume by name
must always run on the same node as that volume's DRBD primary — proven on
initial placement, and again after a hard node kill, not by scheduling
coincidence. This is the first VM test to deploy a block with bound
storage at all, and exercises the whole chain together: the scheduler's
P12 filter (a node is only a placement candidate once it already holds a
healthy replica), the storage controller's movePrimaryForBlock (the
primary then moves to wherever the block actually landed), and the
bridge's bind-mount (§4.7, gated on that same co-location).

Runs after cluster-common.py, block-common.py and vol_cluster.py.
"""

VOL_MIB = 16
MOUNT = "/mnt/share-data"
PORT = 18090
VOL_NAME = "blk-default-share-share-data"  # storage.BlockVolumeName(ns, block, storageName)

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: share
  namespace: default
spec:
  type: util/echo
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 64Mi
  storage:
    - name: share-data
      size: {VOL_MIB * 2}Mi
      replication: 3
      mountPath: {MOUNT}
  config:
    port: {PORT}
    body: "colocated\\n"
"""

ALL_MACHINES = {"n1": n1, "n2": n2, "n3": n3}


def host_mount(m, vol_name):
    """The host-side bind-mount source for vol_name's device, or None if
    the volume isn't visible yet."""
    row = volume_row(m, vol_name)
    if row is None:
        return None
    return f"/var/lib/expanse/volumes/{row['id']}/mnt"


form("colocation")

with subtest("deploy a SINGLETON block bound to a volume"):
    # Generous budget: the volume must be requested, placed, replicated to
    # a healthy state (P12's gate) *before* the block can schedule at all
    # — a real bootstrapping chain, not just a container start.
    deploy(n1, "share", MANIFEST)
    b = wait_phase(n1, "share", ["RUNNING"], 180)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"share placed on {nodes}, want exactly 1: {b.get('status')}"

holder = next(iter(nodes))
hm = ALL_MACHINES[holder]

with subtest("all three replicas reach UpToDate"):
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")

with subtest("the block's node is the volume's primary (D2)"):
    wait_for(lambda: role_of(hm, res) == "Primary", "the block's own node to hold the primary", 60)

with subtest("the volume is mounted on the block's host and holds data"):
    mnt = host_mount(hm, VOL_NAME)
    assert mnt, f"volume {VOL_NAME} not visible on {holder} yet"
    # The mount resource is bridge.go-derived from the volume's primary
    # (a /volumes/ write), which the bridge only watches /blocks/ for —
    # it picks this up on its next periodic tick (DefaultBridgeInterval,
    # 10s), not immediately, so this is a real (bounded) wait, not a
    # bare assertion.
    hm.wait_until_succeeds(f"mountpoint -q {mnt}", timeout=60)
    # sync: a hard crash() can drop unflushed writeback (the same caveat
    # block-singleton.nix/block-reschedule.nix document), and the write
    # must actually reach the DRBD device to be replicated at all.
    hm.succeed(f"echo -n hello-d2 > {mnt}/marker && sync")
    ref = hm.succeed(f"sha256sum {mnt}/marker").split()[0]
    echo_responds(hm, PORT, "colocated", node="localhost")

with subtest("kill the holder's whole VM"):
    ALL_MACHINES[holder].crash()

with subtest("both the block and the volume's primary re-converge on the same new node"):
    # The block was already settled RUNNING before the kill, so
    # wait_phase's "phase in [RUNNING]" would return immediately on a
    # stale, pre-crash read without ever observing the transition —
    # poll the actual placement set directly instead (block-singleton.nix's
    # proven pattern for a kill-after-settle scenario).
    survivor = next(m for n, m in ALL_MACHINES.items() if n != holder)
    deadline = time.time() + 150
    b = None
    new_nodes = set()
    while time.time() < deadline:
        b = get_json(survivor, "share")
        new_nodes = placement_nodes(b) if b else set()
        if new_nodes and holder not in new_nodes:
            break
        time.sleep(2)
    assert new_nodes and holder not in new_nodes, \
        f"share did not move off {holder} within 150 s: {(b or {}).get('status')}"
    new_holder = next(iter(new_nodes))
    new_holder_m = ALL_MACHINES[new_holder]
    # The generic electPrimary (election.go, lowest-healthy-id) can win a
    # tick before movePrimaryForBlock corrects it to the block's actual
    # host on the *next* tick — poll for the specific co-located outcome,
    # not just "any survivor became primary".
    wait_for(lambda: role_of(new_holder_m, res) == "Primary",
             f"the primary to move to {new_holder} (D2's block-host co-location)", 90)

with subtest("the data survived and is reachable at the same host mount path"):
    hm2 = ALL_MACHINES[new_holder]
    mnt2 = host_mount(hm2, VOL_NAME)
    assert mnt2, f"volume {VOL_NAME} not visible on {new_holder}"
    hm2.wait_until_succeeds(f"mountpoint -q {mnt2}", timeout=60)
    got = hm2.succeed(f"sha256sum {mnt2}/marker").split()[0]
    assert got == ref, f"data checksum mismatch after failover: {got} != {ref}"
    echo_responds(hm2, PORT, "colocated", node="localhost")

print("SHARE-COLOCATION DONE")
