"""Probe body for vm-vsock-notify-probe.nix: what a systemd guest sends over vmm.notify_socket.

KERNEL, INITRD, CMDLINE and SYSTEMD_VERSION are spliced in by the .nix wrapper. Prints only.
"""
import shlex
import time

n1.start()
n1.wait_for_unit("multi-user.target")
print(f"guest systemd {SYSTEMD_VERSION}")
print("vhost_vsock before any VM:", n1.execute("lsmod | grep -c vsock || true")[1].strip())


def listen(port, log):
    """One host listener; each guest connection is logged with its peer and arrival time."""
    # SYSTEM's stdout is the connection itself, so the log redirect sits inside it.
    script = (f"exec socat -u VSOCK-LISTEN:{port},fork,reuseaddr SYSTEM:'{{ echo --- conn peer=$SOCAT_PEERADDR "
              f"at $(cut -d\" \" -f1 /proc/uptime); cat; echo; }} >> {log}'")
    n1.succeed(f"systemd-run --unit listen-{port} --setenv=PATH=/run/current-system/sw/bin sh -c {shlex.quote(script)}")


def boot(name, cid, credential, extra=""):
    """Start a guest; returns host uptime at start. Serial goes to /tmp/<name>.serial."""
    smbios = f"-smbios type=11,value=io.systemd.credential:vmm.notify_socket={credential}" if credential else ""
    t = float(n1.succeed("cut -d' ' -f1 /proc/uptime"))
    n1.succeed(
        f"systemd-run --unit vm-{name} qemu-kvm -enable-kvm -nodefaults -display none -no-reboot "
        f"-m 2048 -smp 1 -kernel {KERNEL} -initrd {INITRD} -append '{CMDLINE} {extra}' "
        f"-device vhost-vsock-pci,guest-cid={cid} {smbios} "
        f"-serial file:/tmp/{name}.serial -qmp unix:/tmp/{name}.qmp,server,nowait"
    )
    return t


def wait_text(path, text, timeout):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if n1.execute(f"grep -q '{text}' {path} 2>/dev/null")[0] == 0:
            return True
        time.sleep(1)
    return False


def show(name, t0, log):
    print(f"=== {name}: started at host uptime {t0:.1f} ===")
    print(n1.execute(f"cat {log} 2>/dev/null || echo '(no notify log)'")[1])
    print(n1.execute(f"grep -a PROBE-MARK /tmp/{name}.serial || echo '(no PROBE-MARK)'")[1])
    print(n1.execute(f"systemctl is-active vm-{name}; ls -la /tmp/{name}.serial; tail -n 5 /tmp/{name}.serial")[1])
    print(n1.execute(f"journalctl -u vm-{name} --no-pager -o cat | tail -n 5")[1])
    print(n1.execute("journalctl -u 'listen-*' --no-pager -o cat | tail -n 5")[1])


def stop(name):
    n1.execute(f"systemctl stop vm-{name}")


def qmp(name, cmd):
    return n1.execute(
        f"printf '%s\\n' '{{\"execute\":\"qmp_capabilities\"}}' '{{\"execute\":\"{cmd}\"}}' "
        f"| socat - unix-connect:/tmp/{name}.qmp"
    )[1]


listen(9000, "/tmp/notify-9000.log")

# A: a healthy guest, credential form vsock-stream; then a graceful shutdown.
ta = boot("a", 3, "vsock-stream:2:9000")
print("A READY seen:", wait_text("/tmp/notify-9000.log", "READY=1", 120))
print("vhost_vsock after a VM:", n1.execute("lsmod | grep vsock || true")[1].strip())
time.sleep(5)
show("a", ta, "/tmp/notify-9000.log")
print("A powerdown:", qmp("a", "system_powerdown"))
time.sleep(30)
print("=== A after powerdown ===")
print(n1.execute("cat /tmp/notify-9000.log")[1])

# B: the older credential form vsock:CID:PORT, on its own port.
listen(9001, "/tmp/notify-9001.log")
tb = boot("b", 4, "vsock:2:9001")
print("B READY seen:", wait_text("/tmp/notify-9001.log", "READY=1", 120))
show("b", tb, "/tmp/notify-9001.log")
stop("b")

# C: a service fails during boot: is READY still sent, and is the failure visible?
listen(9002, "/tmp/notify-9002.log")
tc = boot("c", 5, "vsock-stream:2:9002", "probe.fail=1")
print("C READY seen:", wait_text("/tmp/notify-9002.log", "READY=1", 120))
time.sleep(5)
show("c", tc, "/tmp/notify-9002.log")
stop("c")

# D: the guest boots to emergency mode.
listen(9003, "/tmp/notify-9003.log")
td = boot("d", 6, "vsock-stream:2:9003", "systemd.unit=emergency.target")
print("D READY seen:", wait_text("/tmp/notify-9003.log", "READY=1", 90))
show("d", td, "/tmp/notify-9003.log")
stop("d")

# E: no init at all: the kernel panics; nothing should arrive.
listen(9004, "/tmp/notify-9004.log")
te = boot("e", 7, "vsock-stream:2:9004", "init=/nonexistent")
print("E READY seen:", wait_text("/tmp/notify-9004.log", "READY=1", 60))
show("e", te, "/tmp/notify-9004.log")
stop("e")

# F: two guests on one shared port: can the peer CID tell them apart?
listen(9005, "/tmp/notify-9005.log")
tf = boot("f1", 8, "vsock-stream:2:9005")
boot("f2", 9, "vsock-stream:2:9005")
wait_text("/tmp/notify-9005.log", "READY=1", 120)
time.sleep(30)
show("f1", tf, "/tmp/notify-9005.log")
print(n1.execute("tail -n 4 /tmp/f2.serial")[1])

# G: a CID already taken on this host.
rc, out = n1.execute(
    f"timeout 10 qemu-kvm -enable-kvm -nodefaults -display none -m 256 -kernel {KERNEL} "
    "-device vhost-vsock-pci,guest-cid=8 2>&1"
)
print(f"=== G duplicate CID: exit {rc} ===\n{out}")
stop("f1")
stop("f2")

# H: no credential: the guest boots, nothing arrives.
listen(9006, "/tmp/notify-9006.log")
th = boot("h", 10, "")
print("H PROBE-MARK seen:", wait_text("/tmp/h.serial", "PROBE-MARK", 120))
time.sleep(5)
show("h", th, "/tmp/notify-9006.log")
