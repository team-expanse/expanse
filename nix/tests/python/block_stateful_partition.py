"""block-stateful-partition: a stateful block's node is cut off while its unit holds the volume open.

A SINGLETON util/echo block keeps a replication-2 volume bind-mounted into its unit, so the cut-off
node's agent cannot simply unmount and demote. The majority moves the block and its volume primary
elsewhere; every node's DRBD role history must still never show two Primaries at once, and after the
heal the volume reconnects without a split brain and the old node's unit is gone.

Runs after cluster-common.py, block-common.py, vol_cluster.py and vol_role_recorder.py; the wrapper
imports vol_roles as `roles`.
"""

NAME = "keep"
PORT = 18090
VOL = "blk-default-keep-data"  # storage.BlockVolumeName(ns, block, storageName)
UNIT = f"expanse-block@default-{NAME}-0.service"
TOLERANCE_S = 0.05

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {NAME}
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
    - name: data
      size: 64Mi
      replication: 2
      mountPath: /var/lib/keep
  config:
    port: {PORT}
    body: "kept"
"""


def machine(node_id):
    return {m.name: m for m in NODES}[node_id]


def running_on(m):
    """The node running the block as m sees it, or None while it is not RUNNING."""
    b = get_json(m, NAME)
    if b is None or (b.get("status") or {}).get("phase") != "RUNNING":
        return None
    return replica_node(b, 0)


def split_off(m, res):
    return "StandAlone" in drbd_status(m, res)


def dump(res):
    for m in NODES:
        print(f"[{m.name}] drbd:\n{drbd_status(m, res)}")
        print(f"[{m.name}] agent:\n{m.execute('journalctl -u expansed.service -n 40 --no-pager 2>&1')[1]}")


form("bsp")

with subtest("a SINGLETON block holds its replication-2 volume open on one node"):
    deploy(n1, NAME, MANIFEST)
    wait_phase(n1, NAME, ["RUNNING"], 300)
    host = machine(running_on(n1))
    res = volume_row(n1, VOL)["id"]
    wait_for(lambda: role_of(host, res) == "Primary", f"{host.name} to be the volume's primary", 120)
    host.wait_until_succeeds(f"systemctl is-active {UNIT}", timeout=60)
    host.succeed(f"findmnt /var/lib/expanse/volumes/{res}/mnt")
    # The third node joins as a diskless tiebreaker, which turns DRBD quorum on.
    wait_for(lambda: "peer-disk:Diskless" in drbd_status(host, res), "a connected tiebreaker", 120)
    print(f"[{host.name}] drbd with tiebreaker:\n{drbd_status(host, res)}")
    spare = next(m for m in NODES if m.name not in volume_row(n1, VOL)["nodes"])
    inspect = n1.succeed(f"expanse ctl volume inspect {VOL} 2>&1")
    if f"tiebreaker: {spare.name}" not in inspect:
        raise AssertionError(f"inspect does not name {spare.name} as the tiebreaker:\n{inspect}")
    majority = [m for m in NODES if m is not host]
    for m in NODES:
        start_recorder(m)
    offsets = {m.name: offset_of(m) for m in NODES}

with subtest("cutting the host off moves the block and its primary to the majority"):
    began = time.time()
    host.block()
    try:
        wait_for(lambda: running_on(majority[0]) not in (None, host.name), "the block to run on another node", 180)
        wait_for(lambda: len(primaries(res, majority)) == 1, "a primary on the majority side", 120)
        print(f"moved after {time.time() - began:.0f}s; cut-off node role: {role_of(host, res)}")
    except Exception:
        dump(res)
        raise
    finally:
        host.unblock()

with subtest("after the heal the old unit is gone and the volume rejoins without a split brain"):
    try:
        host.wait_until_fails(f"systemctl is-active {UNIT}", timeout=120)
        wait_for(lambda: len(primaries(res)) == 1, "a single primary", 120)
        wait_for(lambda: "peer-disk:UpToDate" in drbd_status(primaries(res)[0], res), "the replicas to resync", 180)
    except Exception:
        dump(res)
        raise
    for m in NODES:
        assert not split_off(m, res), f"{m.name} is split off:\n{drbd_status(m, res)}"

with subtest("no two nodes were ever Primary at once"):
    held = role_history(res, offsets, time.time())
    show(held, began)
    clashes = roles.overlaps(held, TOLERANCE_S)
    if clashes:
        raise AssertionError(f"two nodes were Primary together: {clashes}")
    print("BLOCK-STATEFUL-PARTITION PASSED")
