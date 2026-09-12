# Impermanence: the defining property of Expanse. The root filesystem is
# rolled back to rpool/root@blank on every boot. Anything that survives a
# reboot must live in /persist (bind-mounted here) or in the Nix config.
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
  config = lib.mkIf cfg.enable {
    # Roll back the root dataset before it is mounted. The systemd-initrd
    # variant is preferred: explicit ordering before local-fs.
    boot.initrd.systemd.enable = lib.mkDefault true;
    boot.initrd.systemd.services.expanse-impermanence-rollback = {
      description = "Roll back rpool/root to the blank snapshot";
      wantedBy = [ "initrd.target" ];
      after = [ "zfs-import-rpool.service" ];
      before = [ "sysroot.mount" ];
      unitConfig.DefaultDependencies = "no";
      serviceConfig.Type = "oneshot";
      script = ''
        if zfs list -t snapshot rpool/root@blank >/dev/null 2>&1; then
          echo "expanse: rolling back rpool/root@blank (impermanence)"
          zfs rollback -r rpool/root@blank
        else
          echo "expanse: WARNING rpool/root@blank missing, root will NOT be wiped" >&2
        fi
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
