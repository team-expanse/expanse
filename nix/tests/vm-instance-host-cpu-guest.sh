# Runs inside the guest: write one line naming its vmx flag and /dev/kvm to the start of /dev/vda.
modprobe kvm_intel 2>/dev/null || modprobe kvm_amd 2>/dev/null || true
vmx=0
grep -qwE 'vmx|svm' /proc/cpuinfo && vmx=1
kvm=0
[ -c /dev/kvm ] && kvm=1
printf 'CPU-REPORT vmx=%s kvm=%s END\n' "$vmx" "$kvm" > /dev/vda
sync
