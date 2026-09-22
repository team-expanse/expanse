# Impermanence: the defining property of Expanse. The @root subvolume is
# deleted and recreated from the @root-blank snapshot on every boot,
# including the first (the installer takes that snapshot before
# nixos-install writes anything, so "blank" really is empty -- everything
# the running system needs comes from the untouched @nix subvolume and
# the current generation's activation script). Anything that must survive
# a reboot lives in /persist (bind-mounted here) or in the Nix config.
{ config, pkgs, lib, ... }:
let
  cfg = config.expanse.node;
  persistPath = "/persist";

  # Paths bind-mounted from the persist dataset.
  bindPaths = [
    "/etc/machine-id"
    "/var/lib/nixos"
    "/var/lib/systemd"
    "/root/.ssh"
  ];

  # systemd-escape -p equivalent for mount unit names.
  escapePath = p:
    let
      esc = c:
        if c == "-" then "\\x2d"
        else if c == " " then "\\x20"
        else if c == "/" then "-"
        else c;
    in
    lib.stringAsChars esc (lib.removePrefix "/" p) + ".mount";

  bindMountUnits = map (p: "sysroot-" + escapePath p) bindPaths;
in
{
  options.expanse.node.rootDevice = lib.mkOption {
    type = lib.types.str;
    default = config.fileSystems."/".device;
    description = ''
      Block device holding the btrfs system partition, where @root and
      @root-blank live as top-level siblings. Defaults to whatever disko
      put "/" on; VM tests override it, since the test framework provides
      its own "/" and cannot be repointed at the scratch disk the test
      builds its btrfs filesystem on instead.
    '';
  };

  config = lib.mkIf cfg.enable {
    # Recreate @root from @root-blank before it is mounted. The
    # systemd-initrd variant is preferred: explicit ordering before
    # local-fs.
    boot.initrd.systemd.enable = lib.mkDefault true;
    boot.initrd.systemd.services.expanse-impermanence-rollback = {
      description = "Recreate @root from the @root-blank snapshot";
      wantedBy = [ "initrd.target" ];
      after = [ "systemd-udev-settle.service" ];
      before = [ "sysroot.mount" ];
      unitConfig.DefaultDependencies = "no";
      serviceConfig.Type = "oneshot";
      path = [ pkgs.btrfs-progs pkgs.util-linux pkgs.coreutils ];
      script = ''
        mkdir -p /btrfs-top
        mount -o subvolid=5 "${config.expanse.node.rootDevice}" /btrfs-top
        if [ -d /btrfs-top/@root-blank ]; then
          echo "expanse: recreating @root from @root-blank (impermanence)"
          btrfs subvolume delete -R /btrfs-top/@root
          btrfs subvolume snapshot /btrfs-top/@root-blank /btrfs-top/@root
        else
          echo "expanse: WARNING @root-blank missing, root will NOT be wiped" >&2
        fi
        umount /btrfs-top
      '';
    };

    # Bind mounts from /persist. machine-id must exist before systemd
    # reads it; sources are pre-created by expanse-persist-init below.
    fileSystems = builtins.listToAttrs (map (p: {
      name = p;
      value = {
        device = "${persistPath}${p}";
        fsType = "none";
        options = [ "bind" ];
        neededForBoot = true;
      };
    }) bindPaths);

    # Create bind-mount sources under /persist before the bind mounts
    # start. The bind mounts are neededForBoot, so they happen in systemd
    # stage 1 where units are sysroot-prefixed and /persist is mounted at
    # /sysroot/persist. DefaultDependencies=no plus explicit ordering
    # keeps this running between the /persist mount and the bind mounts.
    boot.initrd.systemd.services.expanse-persist-init = {
      description = "Prepare /persist bind-mount sources";
      wantedBy = bindMountUnits;
      before = bindMountUnits;
      unitConfig = {
        DefaultDependencies = "no";
        RequiresMountsFor = [ "/sysroot${persistPath}" ];
      };
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
      };
      after = [ "sysroot-persist.mount" ];
      script = ''
        mkdir -p /sysroot${persistPath}/var/lib/nixos \
                 /sysroot${persistPath}/var/lib/systemd \
                 /sysroot${persistPath}/root/.ssh \
                 /sysroot${persistPath}/etc
        touch /sysroot${persistPath}/etc/machine-id
        chmod 0700 /sysroot${persistPath}/root/.ssh
      '';
    };

    # Development helper: warn about files written to /etc or /var/lib
    # since boot that are not on the persist list. Never fails.
    systemd.services.expanse-impermanence-check = {
      description = "Report non-persisted state written since boot";
      after = [ "multi-user.target" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
      };
      path = [ pkgs.findutils pkgs.gnugrep ];
      script = ''
        echo "expanse-impermanence-check: files modified since boot outside /persist:"
        find /etc /var/lib -newer /proc/1 -type f 2>/dev/null \
          | grep -v -e '^/etc/machine-id$' \
          || echo "  (none)"
      '';
    };
  };
}
