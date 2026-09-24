# Probe (Phase 6 D1): does this project's own nixosTest harness -- the same
# harness Phase 6's VM tests must run in -- actually pass hardware nested
# virtualization through to an L1 test node? nixosTest's default qemu
# invocation is `-machine accel=kvm:tcg -cpu max` (nixpkgs'
# nixos/lib/qemu-common.nix). If vmx reaches the L1 guest, Phase 6's own VM
# tests can run their guest VMM under real KVM acceleration; if not, every
# Phase 6 VM test nests under software emulation (TCG-on-TCG), a real
# size/timeout risk for Stream A. Observations only; nothing is asserted --
# same role as iscsi-lio-drbd-secondary-probe.nix played for Phase 4 D1.
{ self }:
{ pkgs, lib, ... }:
{
  name = "expanse-vm-nested-kvm-probe";
  nodes.n1 = { ... }: { virtualisation.memorySize = 768; };
  testScript = ''
    n1.start()
    n1.wait_for_unit("multi-user.target")
    print("--- /dev/kvm ---")
    print(n1.succeed("ls -la /dev/kvm || true"))
    print("--- vmx/svm flag in L1 guest cpuinfo ---")
    print(n1.succeed("grep -o -m1 'vmx\\|svm' /proc/cpuinfo || echo NONE"))
    print("--- kvm module loaded in L1 guest ---")
    print(n1.succeed("cat /proc/modules | grep -i kvm || echo 'no kvm module loaded'"))
  '';
}
