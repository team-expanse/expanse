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

    renewalPeriod = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Cert renewal / CA rotation catch-up pass interval (Phase 10 X2). Empty = daemon default (6h).";
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
    # vhost_vsock: vm/instance guests report their boot to expanse-block-run over vsock.
    boot.kernelModules = lib.optional (cfg.storageVG != "") "drbd" ++ [ "vhost_vsock" ];
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

    # Both block unit templates' ReadWritePaths=/var/lib/expanse/volumes
    # (below) must already exist on the host or systemd's own namespace
    # setup fails outright ("Failed at step NAMESPACE") before the unit
    # ever execs — unlike BindPaths, ReadWritePaths does not create a
    # missing target. The mount manager itself only creates this
    # directory once a volume actually lands on this node, so a node
    # hosting no replica yet would otherwise never be able to start any
    # block replica at all.
    systemd.tmpfiles.rules = [
      "d /var/lib/expanse/volumes 0755 root root -"
      # rtslib-fb's dbroot (iscsi/target, PHASE-04-TASKS.md Stream B) —
      # see expanse-block-root@.service's ReadWritePaths comment.
      "d /etc/target 0700 root root -"
      # LIO's PR subsystem opens /etc/target/pr/aptpl_<wwn> on any
      # persistent-reservation registration, not only when APTPL is
      # explicitly requested (PHASE-04-TASKS.md Stream C, X4) --
      # missing this directory surfaces to the initiator as a generic
      # "Device not ready" on PERSISTENT RESERVE OUT.
      "d /etc/target/pr 0700 root root -"
    ];

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
        # Tags every log line so StreamLogs's journalctl --identifier
        # query (internal/blocks/logs) matches it: without this,
        # journald's default SYSLOG_IDENTIFIER is the started binary's
        # own name, not the unit's.
        SyslogIdentifier=expanse-block-%i
        Restart=on-failure
        RestartSec=5s
        DynamicUser=yes
        NoNewPrivileges=yes
        PrivateTmp=yes
        ProtectSystem=strict
        ProtectHome=yes
        # A storage-bound block's own process needs to see its volume's
        # host mount (§4.7) to actually read/write it, not just have it
        # exist on the host — internal/blocks/wire/bridge.go passes the
        # real path via --mount. One fixed root for every replica of
        # every type: this unit is static (shared, never rebuilt per
        # replica), so a per-block exception isn't possible here.
        ReadWritePaths=/var/lib/expanse/volumes
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

    # A second static template, identical except DynamicUser: a handful
    # of workloads (share/smb, share/nfs — PHASE-03-TASKS.md D1/D3) must
    # setuid()/setgid() to the connecting or exported user at runtime, a
    # capability DynamicUser's random unprivileged uid can never hold.
    # Both units are static (systemd.units."<name>@.service" cannot vary
    # DynamicUser per replica from the outside), so
    # internal/blocks/runtime/systemd.UnitNameForSpec routes a
    # RunAsRoot spec here instead of trying to flip this one setting on
    # the shared template.
    systemd.units."expanse-block-root@.service" = {
      enable = true;
      text = ''
        [Unit]
        Description=expanse block %i (root)
        After=network-online.target
        StartLimitIntervalSec=60
        StartLimitBurst=3

        [Service]
        Slice=expanse-blocks.slice
        SyslogIdentifier=expanse-block-%i
        Restart=on-failure
        RestartSec=5s
        NoNewPrivileges=yes
        PrivateTmp=yes
        ProtectSystem=strict
        ProtectHome=yes
        # A storage-bound block's own process needs to see its volume's
        # host mount (§4.7) to actually read/write it — the shared,
        # static ReadWritePaths grant BOTH block unit templates rely on
        # (see the ordinary template's own copy of this comment).
        # /etc/target: rtslib-fb (iscsi/target, PHASE-04-TASKS.md Stream
        # B) refuses to run at all against a missing dbroot directory
        # ("Cannot set dbroot to /etc/target") — this project never uses
        # it to persist config (every targetcli command here is one-shot
        # against live configfs state, not restore/save), but the
        # directory itself must still exist and be writable for rtslib's
        # own RTSRoot() startup check to pass. Only this template grants
        # it: DynamicUser's random uid could never use LIO's root-only
        # configfs tree regardless.
        # /run (== /var/run): targetcli-fb also serializes concurrent
        # invocations through a hardcoded, non-configurable lock file at
        # /var/run/targetcli.lock (unlike its dbroot/prefs paths above,
        # there is no env var to relocate this one) — this project only
        # ever runs one targetcli-driven block type per node in practice
        # (D5: one LUN per instance), so granting the real /run here is
        # a lock-file accommodation, not a meaningfully wider surface for
        # a unit that already runs fully privileged.
        ReadWritePaths=/var/lib/expanse/volumes /etc/target /run
        # targetcli-fb's own shell layer (configshell-fb) separately
        # wants a writable preferences directory, ~/.targetcli by
        # default — root's $HOME is /root, made inaccessible by
        # ProtectHome=yes above, so it fails the same way rtslib's
        # dbroot does without this. Redirected into the one directory
        # this unit already grants, rather than carving out an
        # exception to ProtectHome for /root itself.
        Environment=TARGETCLI_HOME=/etc/target/.targetcli
        # AF_NETLINK: iscsi/target (Stream B) resolves its bound raw
        # volume's device path itself (waitForPrimaryDevice,
        # cmd/expanse-block-run), which queries live DRBD state the same
        # way the agent's own volume engine does (internal/storage/
        # drbd), over a netlink genl socket to the kernel — refused
        # outright without this, the same class of restriction smbd's
        # own AF_NETLINK note (share/smb's module.nix) already covers
        # for interface auto-detection.
        # AF_VSOCK and NotifyAccess: vm/instance hears its guest's boot over vsock
        # and publishes it as this unit's status text, which the agent reads back.
        RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK AF_VSOCK
        NotifyAccess=main
        TasksMax=512
        IOWeight=100
        Environment=PATH=/run/current-system/sw/bin
        # vm/instance's own macvtap uplink (D2, ARCHITECTURE.md A34):
        # the same physical interface the agent's own VIP holder already
        # announces on (empty = auto-detect the default route's device,
        # internal/network/vip.ResolveIface) — reused rather than a
        # second, block-author-declared interface knob, since it answers
        # the identical "which physical NIC is the real one" question.
        Environment=EXPANSE_EXTERNAL_INTERFACE=${cfg.externalInterface}
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
      # manager to util-linux, e2fsprogs and cmp; pgha's promotion seam (Stream B,
      # X2) shells out to psql over a db/postgres replica's own local unix socket --
      # the unit's own PATH is otherwise minimal. The clock-sync health check runs chronyc.
      path = with pkgs; [ lvm2 drbd util-linux e2fsprogs diffutils coreutils postgresql_18 ]
        ++ lib.optional config.services.chrony.enable config.services.chrony.package;
      serviceConfig = {
        Type = "notify";
        NotifyAccess = "main";
        ExecStart = with lib;
          "${pkgs.expanse}/bin/expanse agent --data-dir ${cfg.persistDir}/expanse --period ${cfg.period}" +
          optionalString (cfg.controllerPeriod != "") " --controller-period ${cfg.controllerPeriod}" +
          optionalString (cfg.renewalPeriod != "") " --renewal-period ${cfg.renewalPeriod}" +
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
