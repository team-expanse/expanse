# PHASE-06-TASKS.md Stream B (X2, the phase's decider; X3): a SINGLETON
# vm/instance block survives a hard kill of the node running it -- the
# raw disk's DRBD primary promotes on a survivor, the block scheduler
# reschedules the instance there automatically, and it cold-boots with
# the guest's own filesystem intact, reachable again at its pinned
# macvtap identity (D2) with no client-side reconfiguration. The same
# "slow path" iscsi-target-failover.nix already proved for SINGLETON
# (PHASE-04-TASKS.md Stream C), applied to a VM instead of a LUN.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "vm-instance-failover-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/vm_instance_failover.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';

  guestIP = "192.168.1.50";

  # The same proven netboot-minimal, direct-kernel-boot guest
  # vm-instance.nix already measured working, with one addition: a
  # second, fixed-offset marker that reads its own previous value back
  # and increments it on every boot. The original offset-0 marker
  # surviving a cold restart proves the SAME disk (not a fresh one)
  # came back; the incremented counter proves the guest genuinely
  # rebooted and is alive again, not just that the device is visible.
  l2 = import "${pkgs.path}/nixos" {
    system = pkgs.system;
    configuration = {
      imports = [ "${pkgs.path}/nixos/modules/installer/netboot/netboot-minimal.nix" ];
      netboot.squashfsCompression = "gzip -Xcompression-level 1";
      documentation.enable = lib.mkForce false;
      boot.kernelParams = [ "console=ttyS0" "panic=-1" ];
      networking.usePredictableInterfaceNames = false;
      networking.useDHCP = false;
      networking.interfaces.eth0.ipv4.addresses = [{
        address = guestIP;
        prefixLength = 24;
      }];
      networking.firewall.enable = false;
      systemd.services.canary = {
        description = "write boot markers to the raw disk (X2's own disk-survives-restart proof)";
        wantedBy = [ "multi-user.target" ];
        serviceConfig.Type = "oneshot";
        serviceConfig.RemainAfterExit = true;
        script = ''
          set -eu
          # Original marker (X1, unchanged) at the very start of the disk.
          ${pkgs.coreutils}/bin/printf CANARY-OK > /dev/vda
          # A monotonically increasing per-boot marker at a fixed offset
          # further into the disk: read the last value back, increment,
          # write again -- only possible if this is the SAME disk the
          # previous boot wrote to, not a fresh one. The read is captured
          # into a variable before parsing (not piped straight into grep)
          # -- piping dd directly into grep measurably corrupted the
          # captured value on a real cold-boot re-read.
          raw=$(${pkgs.coreutils}/bin/dd if=/dev/vda bs=1 skip=512 count=8 2>/dev/null)
          prev=$(${pkgs.coreutils}/bin/printf '%s' "$raw" \
                 | ${pkgs.gnugrep}/bin/grep -oE '[0-9]+' || echo 0)
          next=$((prev + 1))
          ${pkgs.coreutils}/bin/printf 'BOOT-%d' "$next" \
            | ${pkgs.coreutils}/bin/dd of=/dev/vda bs=1 seek=512 conv=notrunc 2>/dev/null
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
    expanse.agent.externalInterface = "eth1";
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.qemu_kvm ];
    boot.kernelModules = [ "kvm" ];
    virtualisation.memorySize = 4608;
  };
in
{
  name = "expanse-vm-instance-failover";

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
    ${builtins.readFile ./python/vm_instance_failover.py}
  '';
}
