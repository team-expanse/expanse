# PHASE-06-TASKS.md Stream C (X4): a vm/instance block's GUEST filesystem
# survives a hard kill of its node with zero corruption, under a real
# sustained write load actually in flight at the moment of the kill --
# not a static disk image. This is a strictly stronger, guest-level bar
# than Stream B's own X2 (which only proved the raw, DRBD-replicated
# bytes survive) -- here the guest itself formats ext4, journals, and
# is the one that must fsck clean and account for every acknowledged
# write after an unclean shutdown.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "vm-instance-fs-integrity-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/vm_instance_fs_integrity.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';

  guestIP = "192.168.1.50";
  unitName = "expanse-block-root@default-vm1-0.service";

  # Same proven netboot-minimal, direct-kernel-boot guest image shape as
  # vm-instance(-failover).nix, with a different payload: the guest owns
  # and formats its own ext4 filesystem on the raw disk (host still never
  # touches it -- D3's "opaque, never mounted by the host" stance holds),
  # runs a sustained fsync'd write loop, and on every boot fscks and
  # accounts for every write its own prior boot's /data/.index claims it
  # already acknowledged. Status is printed to the console (ttyS0), which
  # runVM wires straight to the host block unit's own stdout/journal
  # (`-serial stdio`, cmd.Stdout = os.Stdout) -- no new host-side plumbing
  # needed, and it keeps the host from ever mounting the guest's fs itself.
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
        description = "sustained guest filesystem write load (X4's own 'kill mid-write, not after' driver)";
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
  name = "expanse-vm-instance-fs-integrity";

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
    ${builtins.readFile ./python/vm_instance_fs_integrity.py}
  '';
}
