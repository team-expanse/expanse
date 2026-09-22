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

    externalVIPPool = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "External VIP pool (§4.2), e.g. 192.168.1.100-192.168.1.120. Empty = internal VIPs only.";
    };

    externalInterface = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Physical interface to announce external VIPs on. Empty = auto (default route).";
    };

    dnsUpstreams = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "DNS forwarders (T17), comma-separated ip:port. Empty = /etc/resolv.conf.";
    };

    storageVG = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "LVM volume group holding volume replicas. Empty = volume storage disabled.";
    };

    storagePool = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Thin pool inside storageVG. Empty = thick volumes.";
    };

    storageLostAfter = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "How long a node stays gone before its volume replicas are rebuilt elsewhere (e.g. 10m). Empty = the agent default.";
    };

    raftAdvertise = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = ''Raft transport advertised addr (host:port). Must be
        stable across reboots: a node that re-joins after a crash with an
        autodetected (per-boot) address poisons the cluster's leader
        forwarding for every peer. Empty = daemon default (local IP).'';
    };

    firewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Apply the §4.5 nftables ruleset (static skeleton + store-driven dynamic sets).";
    };

    pprofAddr = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Serve net/http/pprof on this addr (e.g. 127.0.0.1:6060) for CPU/heap profiling. Empty = disabled. Debug only, no auth — never expose beyond loopback/a test VM.";
    };
  };

  config = lib.mkIf cfg.enable {
    users.groups.expanse = { };

    # DRBD is out-of-tree and LVM's thin activation is off by default;
    # wire both in, but only on nodes actually configured to hold volume
    # replicas. mkDefault throughout: storage-test.nix sets the identical
    # values itself (a VM test's own concern, not this module's), and must
    # keep winning without a definition conflict.
    boot.extraModulePackages = lib.optional (cfg.storageVG != "") config.boot.kernelPackages.drbd;
    boot.kernelModules = lib.optional (cfg.storageVG != "") "drbd";
    services.drbd = lib.mkIf (cfg.storageVG != "") {
      enable = lib.mkDefault true;
      config = lib.mkDefault ''
        global { usage-count no; }
        include "/etc/drbd.d/*.res";
      '';
    };
    # Resources are brought up by the agent's own reconcile loop, not by the drbd unit.
    systemd.services.drbd.wantedBy = lib.mkIf (cfg.storageVG != "") (lib.mkForce [ ]);
    services.lvm.enable = lib.mkIf (cfg.storageVG != "") (lib.mkDefault true);
    services.lvm.boot.thin.enable = lib.mkIf (cfg.storageVG != "") (lib.mkDefault true);

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
        # No SystemCallFilter: some binary-backed workloads (e.g.
        # node_exporter) use syscall families outside @system-service
        # and die with SIGSYS; the other sandbox options remain.
        TasksMax=512
        IOWeight=100
        # Binary-backed blocks exec upstream binaries (nginx, redis,
        # …) from the system profile (T24 workloads).
        Environment=PATH=/run/current-system/sw/bin
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
      after = [ "network-online.target" ];
      before = [ "expanse-ui.service" ];

      unitConfig = { };
      # The volume runtime shells out to lvm and drbdadm/drbdsetup, and the mount
      # manager to util-linux, e2fsprogs and cmp; the unit's own PATH is minimal.
      path = with pkgs; [ lvm2 drbd util-linux e2fsprogs diffutils coreutils ];
      serviceConfig = {
        Type = "notify";
        NotifyAccess = "main";
        ExecStart = with lib;
          "${pkgs.expanse}/bin/expanse agent --data-dir ${cfg.persistDir}/expanse --period ${cfg.period}" +
          optionalString (cfg.controllerPeriod != "") " --controller-period ${cfg.controllerPeriod}" +
          optionalString (cfg.blocksCatalog != null) " --blocks-catalog ${cfg.blocksCatalog}" +
          optionalString (cfg.blocksFlakeRef != "") " --blocks-flake-ref ${cfg.blocksFlakeRef}" +
          optionalString (cfg.externalVIPPool != "") " --external-vip-pool ${cfg.externalVIPPool}" +
          optionalString (cfg.externalInterface != "") " --external-interface ${cfg.externalInterface}" +
          optionalString (cfg.dnsUpstreams != "") " --dns-upstreams ${cfg.dnsUpstreams}" +
          optionalString (cfg.storageVG != "") " --storage-vg ${cfg.storageVG}" +
          optionalString (cfg.storagePool != "") " --storage-pool ${cfg.storagePool}" +
          optionalString (cfg.storageLostAfter != "") " --storage-lost-after ${cfg.storageLostAfter}" +
          optionalString (cfg.raftAdvertise != "") " --raft-advertise ${cfg.raftAdvertise}" +
          optionalString cfg.firewall " --firewall" +
          optionalString (cfg.pprofAddr != "") " --pprof-addr ${cfg.pprofAddr}";
        # A killed or crashed agent leaves its volumes Primary in the kernel, which would
        # block every other node's promotion; demote whatever is not in use. The leading
        # "-" ignores a failure (a device still open stays Primary, by design).
        ExecStopPost = "-${pkgs.drbd}/bin/drbdadm secondary all";
        Restart = "always";
        RestartSec = "5s";
        # A graceful stop demotes every volume first (up to 30 s each).
        TimeoutStopSec = "60s";
        WatchdogSec = "60s";

        User = "root";
        StateDirectory = "expanse";
        RuntimeDirectory = "expanse";
        RuntimeDirectoryMode = "0755";

        # Hardening (tightened further in Phase 14). sysctl writes need
        # kernel tunables; the store lives under /persist.
        NoNewPrivileges = true;
        # No PrivateTmp or ProtectHome: either gives the unit a private, slave mount namespace,
        # and the volume mounts it makes for the block units would never reach the host.
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
