"""dev-guest (Phase 13 C4): the dev guest image as a vm/instance block with cpuModel host.

Writes the image onto a pre-created volume larger than the image, deploys the block, and from a
second machine (a host cannot reach its own macvtap guest) SSHes in as the dev user. Inside the
guest: nix works, /dev/kvm exists, an L3 kernel boots under qemu -enable-kvm, and / has grown.
Then the restore drill: back up to the client's restic server, lose files, restore them.

Runs after cluster-common.py, block-common.py, vol_cluster.py and single_node_common.py.
Expects IMAGE and GUEST_IP (dev-guest.nix) spliced in ahead of this file.
"""
import shlex

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


# Keepalives end a session the guest stopped answering after ~30 s instead of hanging the test.
# -F /dev/null: ssh refuses system config owned by a non-root user (unprivileged test stores).
SSH = ("ssh -F /dev/null -i /etc/ssh-key -o BatchMode=yes -o StrictHostKeyChecking=no -o ConnectTimeout=5 "
       "-o ServerAliveInterval=5 -o ServerAliveCountMax=6")


def ssh(cmd, timeout=None):
    return client.succeed(f"{SSH} dev@{GUEST_IP} {cmd}", timeout=timeout)


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
        client.wait_until_succeeds(f"ssh -F /dev/null -i /etc/ssh-key -o BatchMode=yes -o StrictHostKeyChecking=no "
                                   f"-o ConnectTimeout=5 dev@{GUEST_IP} true", timeout=600)
    except Exception:
        print(guest_console())
        raise
    # A login with no credentials lists the methods sshd offers: keys only.
    _, out = client.execute(f"ssh -F /dev/null -i /etc/ssh-key -o BatchMode=yes -o StrictHostKeyChecking=no "
                            f"-o PreferredAuthentications=none dev@{GUEST_IP} true 2>&1")
    assert "Permission denied (publickey)." in out, out

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

with subtest("restic backs up /home and /etc/nixos, and a lost file comes back"):
    ssh("systemctl is-active restic-backups-dev.timer")
    ssh(shlex.quote("mkdir -p ~/src/proj ~/.ssh && head -c 1M /dev/urandom > ~/src/proj/data "
                    "&& echo key > ~/.ssh/id_test && sha256sum ~/src/proj/data ~/.ssh/id_test > ~/sums"))
    ssh("sudo systemctl start restic-backups-dev.service", timeout=300)
    print(ssh("sudo restic-dev snapshots"))
    ssh(shlex.quote("rm -rf ~/src ~/.ssh/id_test && ! test -e ~/src"))
    ssh(shlex.quote("sudo restic-dev restore latest --target / --include /home/dev/src "
                    "--include /home/dev/.ssh/id_test"), timeout=300)
    ssh(shlex.quote("sha256sum -c ~/sums"))
    ssh(shlex.quote("sudo restic-dev ls latest /etc/nixos"))

print("DEV-GUEST DONE")
