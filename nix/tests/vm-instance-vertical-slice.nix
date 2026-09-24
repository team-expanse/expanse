# PHASE-06-TASKS.md Stream D: the phase-closing vertical slice -- X3 and
# X4 proven together in ONE run, the way iscsi-vertical-slice.nix closed
# Phase 4. Unlike Stream B/C's own vm-instance-failover.nix/
# vm-instance-fs-integrity.nix, which each explicitly wait on internal
# state (DRBD primary role) before killing, this test stays black-box:
# it deploys, waits only for the guest's own write load to be confirmed
# flowing, and kills whichever node currently holds placement -- "kill
# mid-deploy, not after an assumed internal convergence" (Stream D).
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "vm-instance-vertical-slice-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/vm_instance_vertical_slice.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';

  guestIP = "192.168.1.50";
  unitName = "expanse-block-root@default-vm1-0.service";

  # Identical guest payload to vm-instance-fs-integrity.nix (X4's own
  # proven design): the guest formats and owns a real ext4 filesystem on
  # its raw disk itself, runs a sustained fsync'd write loop, and on
  # every boot fscks and reconciles every write its own prior boot's
  # /data/.index claims it already acknowledged. Reused verbatim rather
  # than re-derived, since it's already measured working (Stream C).
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

      systemd.services.diskinit = {
        description = "format (first boot only), fsck, mount the guest's real filesystem, verify prior writes survived (X4)";
        wantedBy = [ "multi-user.target" ];
        serviceConfig.Type = "oneshot";
        serviceConfig.RemainAfterExit = true;
        serviceConfig.StandardOutput = "journal+console";
        script = ''
          set -eu
          dev=/dev/vda
          if ! ${pkgs.e2fsprogs}/bin/dumpe2fs -h $dev >/dev/null 2>&1; then
            echo "STATUS diskinit format"
            ${pkgs.e2fsprogs}/bin/mkfs.ext4 -F -q $dev
          fi
          set +e
          ${pkgs.e2fsprogs}/bin/e2fsck -fy $dev
          fsck_rc=$?
          set -e
          mkdir -p /data
          ${pkgs.util-linux}/bin/mount $dev /data
          if [ -f /data/.index ]; then
            last=$(cat /data/.index)
            ok=0
            bad=0
            i=1
            while [ "$i" -le "$last" ]; do
              f=/data/file-$i
              if [ -f "$f" ] && ${pkgs.gnugrep}/bin/grep -q "^DATA-$i-" "$f"; then
                ok=$((ok + 1))
              else
                bad=$((bad + 1))
              fi
              i=$((i + 1))
            done
            echo "STATUS verify last=$last ok=$ok bad=$bad fsck_rc=$fsck_rc"
          else
            echo "STATUS verify first-boot fsck_rc=$fsck_rc"
          fi
        '';
      };

      systemd.services.writeload = {
        description = "sustained guest filesystem write load (the vertical slice's own 'kill mid-deploy' driver)";
        after = [ "diskinit.service" ];
        requires = [ "diskinit.service" ];
        wantedBy = [ "multi-user.target" ];
        serviceConfig.Type = "simple";
        serviceConfig.StandardOutput = "journal+console";
        script = ''
          set -eu
          i=$(cat /data/.index 2>/dev/null || echo 0)
          while true; do
            i=$((i + 1))
            printf 'DATA-%d-%s' "$i" "$(${pkgs.coreutils}/bin/date +%s%N)" > /data/file-$i
            ${pkgs.coreutils}/bin/sync
            printf '%d' "$i" > /data/.index
            ${pkgs.coreutils}/bin/sync
            if [ $((i % 5)) -eq 0 ]; then
              echo "STATUS write index=$i"
            fi
            sleep 0.2
          done
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
  name = "expanse-vm-instance-vertical-slice";

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
    UNIT = "${unitName}"
    ${builtins.readFile ./python/vm_instance_vertical_slice.py}
  '';
}
