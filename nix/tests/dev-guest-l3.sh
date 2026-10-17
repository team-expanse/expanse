# Runs inside the dev guest: boot a kernel under KVM with no root fs and wait for its panic.
# qemu fails before any kernel output when KVM is unusable, so the banner proves it ran.
out=$(timeout 240 qemu-kvm -enable-kvm -cpu host -m 256 -nographic -no-reboot \
  -kernel /etc/dev-guest-test/l3-kernel -append "console=ttyS0 panic=-1" 2>&1 || true)
echo "$out" | tail -5
echo "$out" | grep -q "Linux version" || { echo "L3 kernel never printed its banner"; exit 1; }
echo "$out" | grep -q "Kernel panic" || { echo "L3 kernel did not run to its panic"; exit 1; }
echo "L3 OK"
