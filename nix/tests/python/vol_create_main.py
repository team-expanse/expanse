"""vol-create (C3): a DRBD volume is created, mounted by the mount manager, and stays intact.

The mount resource is written by hand where the block bridge would write it. The manager must
format the blank volume once and mount the DRBD device on the primary only; a remount keeps the
filesystem (same UUID, same files); a Secondary is never promoted or mounted; a move-primary
unmounts before it demotes and the new primary sees the same files; and a grown volume's
filesystem is grown by the manager with no resize2fs from the operator.

Runs after cluster-common.py and vol_cluster.py.
"""

import json
import shlex

SIZE_MIB = 256
GROWN_MIB = 512
CREATE_BUDGET_S = 10
MOUNT_BUDGET_S = 120
SOCK = "/run/expanse/agent.sock"
CANARY = "hello from the volume"


def host_path(res):
    return f"/var/lib/expanse/volumes/{res}/mnt"


def mount_key(m, res):
    return f"/node/{m.name}/resources/volume-mount:{res}"


def put_mount(m, res):
    body = json.dumps({"volId": res, "name": "data", "mountPath": "/data", "filesystem": "ext4"})
    value = f"type: volume-mount\n{body}"
    m.succeed(f"expanse ctl kv --socket {SOCK} put {mount_key(m, res)} {shlex.quote(value)}")


def drop_mount(m, res):
    m.succeed(f"expanse ctl kv --socket {SOCK} delete {mount_key(m, res)}")


def mounted_from(m, res):
    return m.execute(f"findmnt -rn -M {host_path(res)} -o SOURCE")[1].strip()


def uuid_of(m, dev):
    return m.succeed(f"blkid -o value -s UUID {dev}").strip()


def fs_mib(m, dev):
    out = m.succeed(f"dumpe2fs -h {dev} 2>/dev/null")
    field = {k.strip(): v.strip() for k, _, v in (ln.partition(":") for ln in out.splitlines())}
    return int(field["Block count"]) * int(field["Block size"]) >> 20


def wait_mounted(m, res, what):
    try:
        wait_for(lambda: mounted_from(m, res) == device_of(m), what, MOUNT_BUDGET_S)
    except Exception:
        print(m.execute("journalctl -u expansed.service --no-pager 2>&1 | grep -iE 'mount|reconcil' | tail -30")[1])
        print(m.execute(f"expanse ctl kv --socket {SOCK} list /node/ 2>&1")[1])
        print(drbd_status(m, res))
        pid = m.succeed("systemctl show -p MainPID --value expansed.service").strip()
        for probe in ("grep drbd /proc/self/mountinfo", f"grep drbd /proc/{pid}/mountinfo"):
            print(f"$ {probe}\n{m.execute(probe + ' 2>&1 || true')[1]}")
        raise


form("volcreate")

with subtest("volume create completes within budget and replicates to every node"):
    start = time.time()
    n1.succeed(f"expanse ctl volume create vcreate --size {SIZE_MIB}Mi --replication 3")
    assert time.time() - start <= CREATE_BUDGET_S, f"volume create took {time.time() - start:.1f}s"
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    wait_for(lambda: len(primaries(res)) == 1, "one primary")
    primary = primaries(res)[0]
    dev = device_of(primary)

with subtest("the initial sync leaves every thin replica unallocated"):
    for m in NODES:
        used = float(m.succeed(f"lvs --noheadings -o data_percent vg0/{res}").strip())
        print(f"{m.name}: {res} {used}% allocated after the initial sync")
        assert used < 10, f"{m.name}: the initial sync filled {used}% of the thin LV"
        pending = m.succeed(f"drbdadm -d adjust {res}").strip()
        assert not pending, f"{m.name}: the kernel's config differs from the file:\n{pending}"

with subtest("the manager formats the blank volume once and mounts the DRBD device"):
    put_mount(primary, res)
    wait_mounted(primary, res, "the DRBD device to be mounted")
    assert primary.succeed(f"blkid -o value -s TYPE {dev}").strip() == "ext4"
    primary.succeed(f"echo '{CANARY}' > {host_path(res)}/canary && sync")
    fs_uuid = uuid_of(primary, dev)

with subtest("a remount keeps the filesystem: same UUID, same files, no reformat"):
    primary.succeed(f"umount {host_path(res)}")
    wait_mounted(primary, res, "the manager to remount")
    assert uuid_of(primary, dev) == fs_uuid, "the filesystem was reformatted"
    assert primary.succeed(f"cat {host_path(res)}/canary").strip() == CANARY

with subtest("a Secondary is never promoted or mounted"):
    other = next(m for m in NODES if m is not primary)
    put_mount(other, res)
    time.sleep(15)  # several reconcile periods
    assert role_of(other, res) == "Secondary", f"{other.name} was promoted"
    assert mounted_from(other, res) == "", f"{other.name} mounted a Secondary"
    assert len(primaries(res)) == 1, [m.name for m in primaries(res)]
    drop_mount(other, res)

with subtest("move-primary unmounts before demoting, and the new primary sees the same files"):
    target = other
    n1.succeed(f"expanse ctl volume move-primary vcreate --to {target.name}")
    wait_for(lambda: primaries(res) == [target], f"{target.name} to become primary", 120)
    assert mounted_from(primary, res) == "", "the old primary kept the volume mounted"
    drop_mount(primary, res)
    put_mount(target, res)
    wait_mounted(target, res, "the new primary to mount")
    assert target.succeed(f"cat {host_path(res)}/canary").strip() == CANARY
    assert uuid_of(target, device_of(target)) == fs_uuid, "failover reformatted the filesystem"
    primary = target

with subtest("a resize grows the mounted filesystem with no operator resize2fs"):
    dev = device_of(primary)
    assert fs_mib(primary, dev) < GROWN_MIB * 3 // 4
    n1.succeed(f"expanse ctl volume resize vcreate --size {GROWN_MIB}Mi")
    wait_for(lambda: fs_mib(primary, dev) >= GROWN_MIB * 3 // 4, "the filesystem to grow", MOUNT_BUDGET_S)
    assert mounted_from(primary, res) == dev, "the volume was unmounted to grow"
    assert primary.succeed(f"cat {host_path(res)}/canary").strip() == CANARY
    print(f"VOL-CREATE PASSED: {fs_mib(primary, dev)} MiB filesystem, data intact across remount, failover and grow")
