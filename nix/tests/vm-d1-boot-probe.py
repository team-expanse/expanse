# Probe body for vm-d1-boot-probe.nix (Phase 6 D1, final choice). Split
# into its own file per CLAUDE.md so it can be linted before a derivation
# build. KERNEL/INITRD/CMDLINE are defined by the .nix wrapper above this.
import time

DISK_CHV = "/tmp/vdb-chv.img"
DISK_QEMU = "/tmp/vdb-qemu.img"

n1.start()
n1.wait_for_unit("multi-user.target")

print("--- guest kernel/initramfs under test (same NixOS build machinery packages.iso uses) ---")
print(KERNEL)
print(INITRD)

print("--- L1's own /dev/kvm and nested virt flag (mirrors vm-nested-kvm-probe.nix) ---")
print(n1.execute("ls -la /dev/kvm || true")[1])
print(n1.execute("grep -o -m1 'vmx\\|svm' /proc/cpuinfo || echo NONE")[1])


def wait_for_canary(path, timeout=90):
    deadline = time.time() + timeout
    while time.time() < deadline:
        rc, _ = n1.execute(f"grep -aq CANARY-OK {path} 2>/dev/null")
        if rc == 0:
            return True
        time.sleep(2)
    return False


print("=== cloud-hypervisor: direct exec, no libvirt ===")
n1.succeed(f"fallocate -l 16M {DISK_CHV}")
n1.succeed(
    "cloud-hypervisor "
    f"--kernel {KERNEL} --initramfs {INITRD} --cmdline '{CMDLINE}' "
    f"--disk path={DISK_CHV} --cpus boot=1 --memory size=512M "
    "--serial file=/tmp/chv-serial.log --console off "
    "--api-socket /tmp/chv-api.sock "
    "> /tmp/chv-stdout.log 2>&1 &"
)
chv_booted = wait_for_canary(DISK_CHV)
print(f"cloud-hypervisor guest wrote its boot marker to the raw disk: {chv_booted}")

print("--- cloud-hypervisor's own stdout/stderr (diagnostic) ---")
print(n1.execute("cat /tmp/chv-stdout.log || true")[1])
print(n1.execute("ls -la /tmp/*.log /tmp/*.sock 2>&1 || true")[1])

print("--- cloud-hypervisor control socket: vm.info over its HTTP API on the unix socket ---")
rc, out = n1.execute("curl -s --unix-socket /tmp/chv-api.sock http://localhost/api/v1/vm.info")
print(f"exit={rc}\n{out}")

print("--- cloud-hypervisor control socket: graceful shutdown via vmm.shutdown ---")
rc, out = n1.execute(
    "curl -s -X PUT --unix-socket /tmp/chv-api.sock http://localhost/api/v1/vmm.shutdown"
)
print(f"exit={rc}\n{out}")
time.sleep(3)
print(n1.execute("pgrep -a cloud-hypervisor || echo 'cloud-hypervisor process gone'")[1])

print("--- cloud-hypervisor serial console tail ---")
print(n1.execute("tail -n 25 /tmp/chv-serial.log || true")[1])

print("=== plain QEMU/KVM: direct exec, no libvirt ===")
n1.succeed(f"fallocate -l 16M {DISK_QEMU}")
n1.succeed(
    "qemu-kvm -enable-kvm -m 2048 -smp 1 "
    f"-kernel {KERNEL} -initrd {INITRD} -append '{CMDLINE}' "
    f"-drive file={DISK_QEMU},if=virtio,format=raw "
    "-serial file:/tmp/qemu-serial.log -display none -no-reboot "
    "-qmp unix:/tmp/qemu-qmp.sock,server,nowait "
    "> /tmp/qemu-stdout.log 2>&1 &"
)
qemu_booted = wait_for_canary(DISK_QEMU)
print(f"qemu guest wrote its boot marker to the raw disk: {qemu_booted}")

print("--- qemu's own stdout/stderr (diagnostic) ---")
print(n1.execute("cat /tmp/qemu-stdout.log || true")[1])
print(n1.execute("ls -la /tmp/*.log /tmp/*.sock 2>&1 || true")[1])

print("--- qemu control socket: QMP query-status over the unix socket ---")
rc, out = n1.execute(
    "printf '%s\\n' '{\"execute\":\"qmp_capabilities\"}' '{\"execute\":\"query-status\"}' "
    "| socat - unix-connect:/tmp/qemu-qmp.sock"
)
print(f"exit={rc}\n{out}")

print("--- qemu control socket: graceful quit via QMP ---")
rc, out = n1.execute(
    "printf '%s\\n' '{\"execute\":\"qmp_capabilities\"}' '{\"execute\":\"quit\"}' "
    "| socat - unix-connect:/tmp/qemu-qmp.sock"
)
print(f"exit={rc}\n{out}")
time.sleep(3)
print(n1.execute("pgrep -a qemu-kvm || echo 'qemu process gone'")[1])

print("--- qemu serial console tail ---")
print(n1.execute("tail -n 25 /tmp/qemu-serial.log || true")[1])

print("--- disk bytes actually written, independent of guest-reported status ---")
print(n1.execute(f"od -c {DISK_CHV} | head -3")[1])
print(n1.execute(f"od -c {DISK_QEMU} | head -3")[1])
