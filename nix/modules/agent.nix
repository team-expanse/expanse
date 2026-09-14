# Expanse node agent (expansed): the systemd units from Phase 02.
#
# expansed is Type=notify: it calls sd_notify(READY=1) after the store
# opens, the socket binds, and the first reconcile completes, and pings
# WATCHDOG=1 from its loops — a wedged agent is restarted automatically.
{ config, pkgs, lib, ... }:
let
  cfg = config.expanse.agent;
in
{
  options.expanse.agent = {
    enable = lib.mkEnableOption "Expanse node agent (expansed)";

    persistDir = lib.mkOption {
      type = lib.types.str;
      default = config.expanse.persistDir or "/persist";
      description = "Persistent state dir for the agent's store.";
    };

    period = lib.mkOption {
      type = lib.types.str;
      default = "30s";
      description = "Base reconcile tick interval.";
    };

    controllerPeriod = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Block placement controller pass interval. Empty = daemon default (30s).";
    };

    blocksCatalog = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Shipped block-type directory (nix/blocks layout). null = block API disabled.";
    };

    blocksFlakeRef = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = ''Flake reference holding block closures (attr per type:
        <category>-<name>). Empty = replicas are not realized on this
        node (API-only member).'';
    };
  };

  config = lib.mkIf cfg.enable {
    users.groups.expanse = { };

    # Block replica runtime (Phase 04 T21): one static template unit —
    # per-replica identity arrives via %i, and the node agent writes the
    # per-replica spec JSON (type, --config args) to
    # /run/expanse/block-replica/<instance>.json before starting the
    # unit, so expanse-block-run can resolve the workload.
    systemd.units."expanse-block@.service" = {
      enable = true;
      text = ''
        [Unit]
        Description=expanse block %i
        After=network-online.target
        StartLimitIntervalSec=60
        StartLimitBurst=3

        [Service]
        Slice=expanse-blocks.slice
        Restart=on-failure
        RestartSec=5s
        DynamicUser=yes
        NoNewPrivileges=yes
        PrivateTmp=yes
        ProtectSystem=strict
        ProtectHome=yes
        RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
        SystemCallFilter=@system-service
        TasksMax=512
        IOWeight=100
        ExecStart=${pkgs.expanse}/bin/expanse-block-run %i
      '';
    };

    systemd.units."expanse-blocks.slice" = {
      enable = true;
      text = ''
        [Slice]
        Description=expanse block replicas
      '';
    };

    systemd.services.expansed = {
      description = "Expanse Node Agent";
      wants = [ "network-online.target" ];
      after = [ "network-online.target" "zfs-mount.service" ];
      before = [ "expanse-ui.service" ];

      unitConfig = { };
      serviceConfig = {
        Type = "notify";
        NotifyAccess = "main";
        ExecStart = with lib;
          "${pkgs.expanse}/bin/expanse agent --data-dir ${cfg.persistDir}/expanse --period ${cfg.period}" +
          optionalString (cfg.controllerPeriod != "") " --controller-period ${cfg.controllerPeriod}" +
          optionalString (cfg.blocksCatalog != null) " --blocks-catalog ${cfg.blocksCatalog}" +
          optionalString (cfg.blocksFlakeRef != "") " --blocks-flake-ref ${cfg.blocksFlakeRef}";
        Restart = "always";
        RestartSec = "5s";
        TimeoutStopSec = "30s";
        WatchdogSec = "60s";

        User = "root";
        StateDirectory = "expanse";
        RuntimeDirectory = "expanse";
        RuntimeDirectoryMode = "0755";

        # Hardening (tightened further in Phase 14). sysctl writes need
        # kernel tunables; the store lives under /persist.
        NoNewPrivileges = true;
        ProtectHome = true;
        PrivateTmp = true;
        ProtectKernelTunables = false;
        RestrictSUIDSGID = true;
        LockPersonality = true;
      };

      wantedBy = [ "multi-user.target" ];
    };

    # Switch watchdog (D2.6): if expansed dies mid `nixos-rebuild switch`
    # (stale /persist/expanse/pending-switch), roll back and reboot so a
    # remote config change can never brick the node.
    systemd.services.expanse-switch-watchdog = {
      description = "Expanse switch watchdog (auto-rollback on failed switch)";
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${pkgs.expanse}/bin/expanse watchdog";
        User = "root";
      };
    };

    systemd.timers.expanse-switch-watchdog = {
      description = "Run the expanse switch watchdog every minute";
      timerConfig = {
        OnBootSec = "2min";
        OnUnitActiveSec = "1min";
      };
      wantedBy = [ "timers.target" ];
    };
  };
}
