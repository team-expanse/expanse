"""ui-vm-cloud-image (Phase 14 F): a vm/instance deployed from the web form with a custom image URL
and a checksum file. The node downloads and checks the qcow2, writes it onto the blank volume and
boots it with a cloud-init seed; the guest takes its address by DHCP and lets the given user in
with the given key under the block's name. A restart boots the same disk without writing it again.
Runs after cluster-common.py, block-common.py, vol_cluster.py, single_node_common.py and
ui_forms_common.py. Expects IMAGE and PUBKEY (ui-vm-cloud-image.nix) spliced in ahead of this file.
"""

import html

PASSWORD = "ui-vm-test-password"
NAME = "vm1"
UNIT = f"expanse-block-root@default-{NAME}-0.service"
LEASES = "/var/lib/dnsmasq/dnsmasq.leases"
# Keepalives end a session the guest stopped answering instead of hanging the test.
SSH = ("ssh -F /dev/null -i /etc/ssh-key -o BatchMode=yes -o StrictHostKeyChecking=no "
       "-o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=6")


def journal():
    return n1.execute(f"journalctl -u {UNIT} --no-pager -o cat 2>&1")[1]


def guest_ip(mac):
    rc, out = client.execute(f"grep -i ' {mac} ' {LEASES}")
    return out.split()[2] if rc == 0 and out.strip() else ""


def ssh(ip, cmd):
    return client.succeed(f"{SSH} tester@{ip} {shlex.quote(cmd)}", timeout=300)


form_single("vmcloud")
client.start()
n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")
csrf = ui_login()

with subtest("the LAN serves the image and its checksum file"):
    client.wait_for_unit("dnsmasq.service")
    client.succeed(f"mkdir -p /srv/img && ln -sf {IMAGE} /srv/img/nixos.qcow2")
    client.succeed("cd /srv/img && sha256sum nixos.qcow2 > SHA256SUMS")
    client.succeed("systemd-run --unit img-http -p WorkingDirectory=/srv/img python3 -m http.server 8000")
    client.wait_for_open_port(8000)
    n1.wait_until_succeeds("curl -sf -o /dev/null http://client:8000/SHA256SUMS", timeout=60)

with subtest("the form deploys the VM from a custom image"):
    status, _, body = post("/deploy/vm/instance", {
        "name": NAME, "cpu": "2", "memory": "1Gi", "storage/disk/size": "8Gi",
        "cfg/image@preset": "custom",
        "cfg/image/url": "http://client:8000/nixos.qcow2",
        "cfg/image/checksumUrl": "http://client:8000/SHA256SUMS",
        "cfg/cloudInit/user": "tester",
        "cfg/cloudInit/sshKeys": PUBKEY,
    })
    assert status == "303", f"deploy: {status}\n{body}"

with subtest("the block page shows the guest's MAC"):
    page = n1.succeed(f"{CURL} https://n1:8443/blocks/default/{NAME}")
    found = re.search(r"<dt>MAC address</dt><dd><code>([0-9a-f:]{17})</code>", page)
    assert found, "no MAC on the block page"
    MAC = html.unescape(found.group(1))

with subtest("the image is written once and the guest boots"):
    try:
        wait_phase(n1, NAME, ["RUNNING"], 2400)
    except Exception:
        print(journal()[-6000:])
        for cmd in (f"expanse ctl block explain {NAME}", "expanse ctl kv get /nodes/n1/capabilities",
                    "expanse ctl kv get /nodes/n1/capacity", "expanse ctl kv list /storage-nodes/", "ls -l /dev/kvm"):
            print(cmd, "->", n1.execute(cmd + " 2>&1")[1])
        dev = device_of(n1)
        print(n1.execute(
            "img=$(ls /var/lib/expanse/images/ | grep -v '^\\.' | head -1); "
            f"qemu-img convert -f qcow2 -O raw /var/lib/expanse/images/$img /tmp/ref.raw && "
            f"cmp -n $(stat -c %s /tmp/ref.raw) /tmp/ref.raw {dev} | head -3; echo cmp-rc=$?; "
            f"loop=$(losetup -r -P -f --show {dev}); blkid -p $loop ${{loop}}p1; losetup -d $loop 2>&1")[1])
        raise
    log = journal()
    assert "image http://client:8000/nixos.qcow2 written" in log, log[-4000:]

with subtest("cloud-init made the user, the key and the hostname"):
    wait_for(lambda: guest_ip(MAC) != "", f"a DHCP lease for {MAC}", 300)
    IP = guest_ip(MAC)
    client.wait_until_succeeds(f"{SSH} tester@{IP} true", timeout=300)
    assert ssh(IP, "hostname").strip() == NAME
    size = int(ssh(IP, "df -B1 --output=size / | tail -1").strip())
    assert size > 6 << 30, f"root fs not grown to the 8Gi disk: {size}"
    # Only the marker: a full sync also flushes the root resize, slowly when nested.
    ssh(IP, "echo kept > marker && sync marker")

with subtest("a restart boots the same disk without writing the image again"):
    boot_id = ssh(IP, "cat /proc/sys/kernel/random/boot_id").strip()
    n1.succeed(f"systemctl restart {UNIT}")
    wait_for(lambda: client.execute(f"{SSH} tester@{IP} cat /proc/sys/kernel/random/boot_id")[1].strip() not in ("", boot_id),
             "the guest back on a new boot", 600)
    assert ssh(IP, "cat marker").strip() == "kept"
    assert journal().count("image http://client:8000/nixos.qcow2 written") == 1, "the image was written again"

print("UI-VM-CLOUD-IMAGE DONE")
