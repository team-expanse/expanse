"""vm-instance-large (Phase 13 D1): a 12-vCPU, 32 GiB guest on a 256 GiB volume.

Every axis is above the scheduler's old fixed node capacity, so the block places only if the
agent's published capacity reaches the scheduler. The guest must see its full size, keep a synced
file across a block restart, and grow / to the volume. Times to RUNNING and to SSH are printed.

Runs after cluster-common.py, block-common.py, vol_cluster.py and single_node_common.py.
Expects IMAGE and GUEST_IP (vm-instance-large.nix) spliced in ahead of this file.
"""

import shlex

NAME = "big"
VOL_NAME = f"blk-default-{NAME}-disk"  # storage.BlockVolumeName: the block adopts this volume
VOL_SIZE_GIB = 256
VCPUS = 12
MEM_GIB = 32
UNIT = f"expanse-block-root@default-{NAME}-0.service"

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {NAME}
  namespace: default
spec:
  type: vm/instance
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: {VCPUS * 1000}m
      memory: {MEM_GIB}Gi
  storage:
    - name: disk
      size: {VOL_SIZE_GIB}Gi
      replication: 1
      mountPath: /mnt/{NAME}-disk
      filesystem: none
  placement:
    requiredCapabilities: [kvm]
  config:
    cpuModel: host
"""


# Keepalives end a session the guest stopped answering after ~30 s instead of hanging the test.
SSH = ("ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o ConnectTimeout=5 "
       "-o ServerAliveInterval=5 -o ServerAliveCountMax=6")


def ssh(cmd, timeout=None):
    # Quoted whole, so redirects and && run in the guest, not on the client.
    return client.succeed(f"{SSH} dev@{GUEST_IP} {shlex.quote(cmd)}", timeout=timeout)


def wait_ssh():
    try:
        client.wait_until_succeeds(f"ssh -o BatchMode=yes -o StrictHostKeyChecking=no "
                                   f"-o ConnectTimeout=5 dev@{GUEST_IP} true", timeout=600)
    except Exception:
        print(n1.execute(f"journalctl -u {UNIT} --no-pager -o cat 2>&1 | tail -60")[1])
        raise


form_single("vmlarge")
client.start()

with subtest("the node publishes its real capacity"):
    n1.wait_until_succeeds("expanse ctl kv get /nodes/n1/capacity | grep -q 'disk='", timeout=120)
    print(n1.succeed("expanse ctl kv get /nodes/n1/capacity"))

with subtest("pre-create the block's volume and write the dev image onto it"):
    n1.succeed(f"expanse ctl volume create {VOL_NAME} --size {VOL_SIZE_GIB}Gi --replication 1")
    n1.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: role_of(n1, res) == "Primary", "n1 primary for the volume", 180)
    n1.succeed(f"dd if={IMAGE} of={device_of(n1)} bs=4M conv=sparse oflag=direct status=none")
    n1.succeed(f"sync {device_of(n1)}")

with subtest("the large block places, boots and answers SSH"):
    t = time.time()
    deploy(n1, NAME, MANIFEST)
    wait_phase(n1, NAME, ["RUNNING"], 900)
    print(f"RUNNING {time.time() - t:.1f}s after apply")
    wait_ssh()
    print(f"SSH {time.time() - t:.1f}s after apply")

with subtest("the guest sees every vCPU, its memory and a grown root"):
    assert int(ssh("nproc").strip()) == VCPUS
    mem_kib = int(ssh("grep MemTotal /proc/meminfo").split()[1])
    assert mem_kib > (MEM_GIB - 2) << 20, f"MemTotal {mem_kib} KiB"
    size = int(ssh("df -B1 --output=size / | tail -1").strip())
    print(f"root fs {size >> 30} GiB")
    assert size > (VOL_SIZE_GIB - 8) << 30, f"root fs not grown: {size}"

with subtest("a synced file survives a block restart"):
    ssh("head -c 64M /dev/urandom > marker && sha256sum marker > marker.sum && sync")
    boot_id = ssh("cat /proc/sys/kernel/random/boot_id").strip()
    t = time.time()
    n1.succeed(f"systemctl restart {UNIT}")
    wait_phase(n1, NAME, ["RUNNING"], 600)
    wait_for(lambda: client.execute(
        f"ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o ConnectTimeout=5 dev@{GUEST_IP} "
        "cat /proc/sys/kernel/random/boot_id")[1].strip() not in ("", boot_id),
        "the guest back on a new boot", 600)
    print(f"SSH back {time.time() - t:.1f}s after restart")
    # The guest is still finishing boot here; under load the first session can drop.
    client.wait_until_succeeds(f"{SSH} dev@{GUEST_IP} 'sha256sum -c marker.sum'", timeout=120)

print("VM-INSTANCE-LARGE DONE")
