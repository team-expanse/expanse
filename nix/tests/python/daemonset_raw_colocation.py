"""PHASE-04-TASKS.md Stream A: a DAEMONSET block bound to storage must place
only on nodes holding a healthy replica of it (D2, generalizing PHASE-03-
TASKS.md's SINGLETON-only P12 filter), and a storage entry declared
filesystem: none must be handed the bare DRBD device, never formatted or
mounted as a directory (D3). The first VM test to combine DAEMONSET with
bound storage at all (placeDaemonset never called the scheduler before this
stream — the P12 generalization was unreachable from the real control path
until it did).

The exclusion half of D2 (a node genuinely lacking a healthy replica never
gets a placement) is proven at the controller-test level
(TestDaemonsetRespectsVolumeColocation), which can inject that boundary
condition directly and deterministically; this VM test proves the positive,
foundational case against a real cluster: once every node's replica is
genuinely healthy, every node gets placed, and the raw device it is handed
is never touched as a filesystem.

Runs after cluster-common.py, block-common.py and vol_cluster.py.
"""

VOL_MIB = 16
PORT = 18091
VOL_NAME = "blk-default-lun-lun"  # storage.BlockVolumeName(ns, block, storageName)

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: lun
  namespace: default
spec:
  type: util/echo
  strategy:
    kind: DAEMONSET
  resources:
    requests:
      cpu: 100m
      memory: 64Mi
  storage:
    - name: lun
      size: {VOL_MIB * 2}Mi
      replication: 3
      mountPath: /mnt/lun
      filesystem: none
  config:
    port: {PORT}
    body: "daemonset-raw\\n"
"""

ALL_MACHINES = {"n1": n1, "n2": n2, "n3": n3}


def host_mount(m, vol_name):
    row = volume_row(m, vol_name)
    if row is None:
        return None
    return f"/var/lib/expanse/volumes/{row['id']}/mnt"


form("dsraw")

with subtest("deploy a DAEMONSET block bound to a raw volume"):
    deploy(n1, "lun", MANIFEST)
    b = wait_phase(n1, "lun", ["RUNNING"], 180)
    nodes = placement_nodes(b)
    assert nodes == {"n1", "n2", "n3"}, \
        f"daemonset placed on {nodes}, want all 3 nodes: {b.get('status')}"

with subtest("all three replicas reach UpToDate"):
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")

with subtest("exactly one node holds the DRBD primary"):
    wait_for(lambda: len(primaries(res)) == 1, "exactly one primary to emerge", 60)
    prim = primaries(res)[0]

with subtest("the raw device resolves on the primary but is never mounted or formatted"):
    mnt = host_mount(prim, VOL_NAME)
    assert mnt, f"volume {VOL_NAME} not visible on {prim.name} yet"
    dev = "/dev/" + prim.succeed("ls /dev | grep -E '^drbd[0-9]+$' | head -1").strip()
    # A raw entry's attach() returns as soon as the device resolves under
    # Primary, before ever creating the host mount directory (D3) — so
    # `mountpoint` correctly fails here whether or not the path exists at
    # all, which is itself part of what this proves: nothing is mounted.
    rc, _ = prim.execute(f"mountpoint -q {mnt}")
    assert rc != 0, f"a raw entry's host path must never be mounted: {mnt} on {prim.name}"
    rc, out = prim.execute(f"blkid -o value -s TYPE {dev}")
    assert rc != 0 or out.strip() == "", \
        f"a raw entry's device must never be formatted, but blkid sees {out!r} on {dev}"

with subtest("every node's own daemonset replica is reachable"):
    for m in NODES:
        echo_responds(m, PORT, "daemonset-raw", node="localhost")

print("DAEMONSET-RAW-COLOCATION DONE")
