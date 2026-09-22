# Shared test helper: create the system btrfs filesystem (subvolumes +
# blank snapshot) on a fresh scratch disk on first boot, inside systemd
# stage 1, mirroring storage.nix + impermanence.nix's rollback service.
#
# The VM test framework mounts the VM's own root disk as / (it overrides
# the whole fileSystems attrset; extra mounts must be declared via
# virtualisation.fileSystems). @root is therefore mounted at /btrfs-root
# and the tests assert the impermanence rollback wipes it on every boot
# while /persist survives. All impermanence mechanisms (rollback service,
# persist-init, bind mounts) run exactly as in production; only the
# device the rollback service targets is overridden, since the framework
# cannot be repointed at this scratch disk for its own "/".
{ lib, pkgs, ... }:
let
  dev = "/dev/vdb";
in
{
  expanse.node.rootDevice = dev;

  virtualisation.fileSystems = {
    "/persist" = { device = dev; fsType = "btrfs"; options = [ "subvol=@persist" ]; neededForBoot = true; };
    "/var/log" = { device = dev; fsType = "btrfs"; options = [ "subvol=@log" ]; };
    "/btrfs-root" = { device = dev; fsType = "btrfs"; options = [ "subvol=@root" ]; neededForBoot = true; };
    "/etc/machine-id" = {
      device = "/persist/etc/machine-id";
      fsType = "none";
      options = [ "bind" ];
      neededForBoot = true;
    };
    "/var/lib/nixos" = {
      device = "/persist/var/lib/nixos";
      fsType = "none";
      options = [ "bind" ];
      neededForBoot = true;
    };
    "/var/lib/systemd" = {
      device = "/persist/var/lib/systemd";
      fsType = "none";
      options = [ "bind" ];
      neededForBoot = true;
    };
    "/root/.ssh" = {
      device = "/persist/root/.ssh";
      fsType = "none";
      options = [ "bind" ];
      neededForBoot = true;
    };
  };

  boot.initrd.systemd.services.expanse-test-pool-init = {
    description = "Create the test btrfs filesystem on first boot";
    wantedBy = [ "initrd.target" ];
    before = [
      "expanse-impermanence-rollback.service"
      "sysroot-btrfs\\x2droot.mount"
      "sysroot-persist.mount"
      "sysroot-var-log.mount"
    ];
    # systemd-udev-settle.service isn't pulled into this minimal initrd; the
    # scratch disk's own device unit is what actually exists to wait on.
    after = [ "dev-vdb.device" ];
    requires = [ "dev-vdb.device" ];
    unitConfig.DefaultDependencies = "no";
    serviceConfig.Type = "oneshot";
    path = [ pkgs.btrfs-progs pkgs.util-linux pkgs.coreutils ];
    script = ''
      if ! blkid -o value -s TYPE ${dev} >/dev/null 2>&1; then
        echo "expanse-test: creating btrfs on ${dev}"
        mkfs.btrfs -f ${dev}
        mkdir -p /btrfs-top
        mount -o subvolid=5 ${dev} /btrfs-top
        for sv in @root @nix @persist @log; do
          btrfs subvolume create /btrfs-top/$sv
        done
        btrfs subvolume snapshot -r /btrfs-top/@root /btrfs-top/@root-blank
        umount /btrfs-top
      fi
    '';
  };
}
