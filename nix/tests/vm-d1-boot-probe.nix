# Probe (Phase 6 D1, final choice): D1's remaining question -- cloud-hypervisor
# vs. plain QEMU/KVM -- was explicitly left to "a real glue-code prototype"
# (`PHASE-06-TASKS.md` D1), not another adoption-test probe alone. This is
# that prototype's measurement half: exec both VMMs directly (no libvirt,
# the same rejection class Phase 5 D1 gave Patroni), each booting the exact
# same minimal NixOS guest (built via the same in-tree NixOS module
# machinery `packages.iso` already uses) straight from --kernel/--initramfs,
# with a raw virtio-blk disk attached the same way a DRBD volume would be.
# Observations only; nothing is asserted -- same role as
# iscsi-lio-drbd-secondary-probe.nix played for Phase 4 D1.
{ self }:
{ pkgs, lib, ... }:
let
  # A minimal netboot-style guest: tmpfs root, no bootloader, no disk
  # needed for the guest's own root filesystem -- the only real disk
  # attached (/dev/vdb) stands in for a VM's actual raw-volume-backed
  # disk. Built via the exact same NixOS module this repo's own
  # `packages.iso` target already builds successfully.
  l2 = import "${pkgs.path}/nixos" {
    system = pkgs.system;
    configuration = {
      imports = [ "${pkgs.path}/nixos/modules/installer/netboot/netboot-minimal.nix" ];
      # Level 19 zstd is archival-grade; this image is thrown away
      # after one probe run, so trade compression ratio for build time.
      netboot.squashfsCompression = "gzip -Xcompression-level 1";
      documentation.enable = lib.mkForce false;
      boot.kernelParams = [ "console=ttyS0" "panic=-1" ];
      # Writes a fixed marker to the raw disk (the guest's own analog
      # of X4's "write under load" check, in miniature) then powers
      # off -- no interactive console needed to observe a real boot.
      systemd.services.canary = {
        description = "write a boot marker to the raw disk, then power off";
        wantedBy = [ "multi-user.target" ];
        serviceConfig.Type = "oneshot";
        # The only disk attached (a single virtio-blk device) is /dev/vda,
        # not /dev/vdb -- confirmed the hard way: a first attempt targeting
        # /dev/vdb wrote nothing (no such device) but the guest still
        # powered off cleanly, since a plain (non-"set -e") shell script
        # doesn't abort on one failed redirect.
        script = ''
          ${pkgs.coreutils}/bin/printf CANARY-OK > /dev/vda
          ${pkgs.coreutils}/bin/sync
          ${pkgs.systemd}/bin/systemctl --no-block poweroff
        '';
      };
    };
  };
  kernel = "${l2.config.system.build.kernel}/${l2.config.system.boot.loader.kernelFile}";
  initrd = "${l2.config.system.build.netbootRamdisk}/initrd";
  cmdline = "init=${l2.config.system.build.toplevel}/init ${toString l2.config.boot.kernelParams}";
in
{
  name = "expanse-vm-d1-boot-probe";
  nodes.n1 = { ... }: {
    virtualisation.memorySize = 4096;
    environment.systemPackages = [ pkgs.cloud-hypervisor pkgs.qemu_kvm pkgs.socat pkgs.curl ];
  };
  testScript = ''
    KERNEL = "${kernel}"
    INITRD = "${initrd}"
    CMDLINE = "${cmdline}"
  '' + builtins.readFile ./vm-d1-boot-probe.py;
}
