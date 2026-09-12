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
  };

  config = lib.mkIf cfg.enable {
    users.groups.expanse = { };

    systemd.services.expansed = {
      description = "Expanse Node Agent";
      wants = [ "network-online.target" ];
      after = [ "network-online.target" "zfs-mount.service" ];
      before = [ "expanse-ui.service" ];

      unitConfig = { };
      serviceConfig = {
        Type = "notify";
        NotifyAccess = "main";
        ExecStart = "${pkgs.expanse}/bin/expanse agent --data-dir ${cfg.persistDir}/expanse --period ${cfg.period}";
        Restart = "always";
        RestartSec = "5s";
        TimeoutStopSec = "30s";
        WatchdogSec = "60s";

        User = "root";
        StateDirectory = "expanse";
        RuntimeDirectory = "expanse";
        RuntimeDirectoryMode = "0750";

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
