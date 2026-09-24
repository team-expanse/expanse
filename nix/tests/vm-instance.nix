# PHASE-06-TASKS.md Stream A, X1: deploy a SINGLETON vm/instance block
# bound to a raw DRBD-backed volume through the real block pipeline (not
# a standalone probe) -- the actual `expanse ctl block apply` path drives
# internal/blocks/wire/bridge.go's replicaSpec, cmd/expanse-block-run's
# runVM, macvtap creation and qemu-kvm, exactly the way a real deploy
# would. D1 (plain QEMU/KVM, `ARCHITECTURE.md` A33 revised) and D2
# (macvtap, A34) are both exercised for real here, not just their own
# standalone probes.
#
# D4 leaves getting a first bootable guest OS onto an empty raw volume
# out of this phase's own scope (document, don't build): the guest's own
# root filesystem is the same proven netboot-minimal, direct-kernel-boot
# image nix/tests/vm-d1-boot-probe.nix already measured working (kernel
# and initramfs supplied directly, not read from the disk); the raw
# volume is attached as the guest's real, persistent virtio-blk disk
# (X1's "disk on a replicated raw volume"), which a real systemd service
# inside the guest writes a marker to, mirroring the D1 probe's own
# canary pattern.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "vm-instance-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/vm_instance.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';

  guestIP = "192.168.1.50";

  # The exact guest shape vm-d1-boot-probe.nix already measured booting
  # successfully under plain qemu-kvm -- deliberately not re-risked here.
  # The only addition is a real network identity (a static address on
  # the macvtap-bridged LAN segment, D2) and leaving the guest running
  # (no self-poweroff) so this test can prove X1's own "reachable"
  # requirement, not just "booted".
  l2 = import "${pkgs.path}/nixos" {
    system = pkgs.system;
    configuration = {
      imports = [ "${pkgs.path}/nixos/modules/installer/netboot/netboot-minimal.nix" ];
      netboot.squashfsCompression = "gzip -Xcompression-level 1";
      documentation.enable = lib.mkForce false;
      boot.kernelParams = [ "console=ttyS0" "panic=-1" ];
      # A single virtio-net device, named predictably: this config is
      # built standalone (not through nixosTest's own node wrapper,
      # which sets this for n1/n2/n3 themselves), so it must set its own
      # "eth0" convention rather than inherit udev's default enpXsY name.
      networking.usePredictableInterfaceNames = false;
      networking.useDHCP = false;
      networking.interfaces.eth0.ipv4.addresses = [{
        address = guestIP;
        prefixLength = 24;
      }];
      networking.firewall.enable = false;
      systemd.services.canary = {
        description = "write a boot marker to the raw disk (X1's own persistent disk)";
        wantedBy = [ "multi-user.target" ];
        serviceConfig.Type = "oneshot";
        serviceConfig.RemainAfterExit = true;
        # /dev/vda: the block's one storage entry is this guest's only
        # attached disk (vm-d1-boot-probe.nix's own finding -- a single
        # virtio-blk device is /dev/vda, not /dev/vdb).
        script = ''
          ${pkgs.coreutils}/bin/printf CANARY-OK > /dev/vda
          ${pkgs.coreutils}/bin/sync
        '';
      };
    };
  };
  kernel = "${l2.config.system.build.kernel}/${l2.config.system.boot.loader.kernelFile}";
  initrd = "${l2.config.system.build.netbootRamdisk}/initrd";
  cmdline = "init=${l2.config.system.build.toplevel}/init ${toString l2.config.boot.kernelParams}";

  nodeCommon = idx: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
    ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    expanse.storage-test.enable = true;
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    # D2's own macvtap uplink (A34): "auto" default-route detection does
    # not work on this harness's flat-LAN test nodes (no gateway, only a
    # directly-connected subnet route) -- every VIP-using block test
    # already sets this for the identical reason.
    expanse.agent.externalInterface = "eth1";
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.qemu_kvm ];
    boot.kernelModules = [ "kvm" ];
    # D1's own measured finding (vm-d1-boot-probe.nix): the netboot
    # initramfs's embedded squashfs (~613 MiB) needs the guest's own -m
    # comfortably above its size to unpack without OOM-panicking, and
    # the L1 host needs real headroom on top of that to run the nested
    # L2 guest at all. Every node gets the same budget: SINGLETON's
    # first-ever placement is an arbitrary first-primary election, so
    # any of the three could end up hosting the VM instance.
    virtualisation.memorySize = 4608;
  };
in
{
  name = "expanse-vm-instance";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    KERNEL = "${kernel}"
    INITRD = "${initrd}"
    CMDLINE = "${cmdline}"
    GUEST_IP = "${guestIP}"
    ${builtins.readFile ./python/vm_instance.py}
  '';
}
