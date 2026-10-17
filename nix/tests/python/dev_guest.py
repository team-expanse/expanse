"""dev-guest (Phase 13 C4): the dev guest image as a vm/instance block with cpuModel host.

Writes the image onto a pre-created volume larger than the image, deploys the block, and from a
second machine (a host cannot reach its own macvtap guest) SSHes in as the dev user. Inside the
guest: nix works, /dev/kvm exists, an L3 kernel boots under qemu -enable-kvm, and / has grown.

Runs after cluster-common.py, block-common.py, vol_cluster.py and single_node_common.py.
Expects IMAGE and GUEST_IP (dev-guest.nix) spliced in ahead of this file.
"""

NAME = "dev"
VOL_NAME = f"blk-default-{NAME}-disk"  # storage.BlockVolumeName: the block adopts this volume
VOL_SIZE_GIB = 12
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
      cpu: 2000m
      memory: 3Gi
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


def ssh(cmd, timeout=None):
    return client.succeed(f"ssh -o BatchMode=yes -o StrictHostKeyChecking=no dev@{GUEST_IP} {cmd}",
                          timeout=timeout)


def guest_console():
    return n1.execute(f"journalctl -u {UNIT} --no-pager -o cat 2>&1 | tail -60")[1]


form_single("devguest")
client.start()

with subtest("pre-create the block's volume and write the dev image onto it"):
    n1.succeed(f"expanse ctl volume create {VOL_NAME} --size {VOL_SIZE_GIB}Gi --replication 1")
    n1.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: role_of(n1, res) == "Primary", "n1 primary for the volume", 180)
    t = time.time()
    n1.succeed(f"dd if={IMAGE} of={device_of(n1)} bs=4M conv=sparse oflag=direct status=none")
    n1.succeed(f"sync {device_of(n1)}")
    print(f"image written in {time.time() - t:.0f}s")

with subtest("the block boots the dev guest and it answers SSH from another machine"):
    deploy(n1, NAME, MANIFEST)
    wait_phase(n1, NAME, ["RUNNING"], 900)
    try:
        client.wait_until_succeeds(f"ssh -o BatchMode=yes -o StrictHostKeyChecking=no "
                                   f"-o ConnectTimeout=5 dev@{GUEST_IP} true", timeout=600)
    except Exception:
        print(guest_console())
        raise

with subtest("nix works and the guest has /dev/kvm"):
    print(ssh("nix --version"))
    ssh("test -c /dev/kvm")
    ssh("grep -qw vmx /proc/cpuinfo")

with subtest("an L3 kernel boots under qemu -enable-kvm inside the guest"):
    print(ssh("dev-guest-l3-check", timeout=300))

with subtest("the root filesystem grew to the volume"):
    size = int(ssh("df -B1 --output=size / | tail -1").strip())
    image = int(n1.succeed(f"stat -c %s {IMAGE}").strip())
    print(f"image {image >> 20} MiB, root fs {size >> 20} MiB")
    assert size > image + (2 << 30) and size > (VOL_SIZE_GIB - 2) << 30, f"root fs not grown: {size}"

print("DEV-GUEST DONE")
