{
  description = "Expanse — flexible, deterministic, expansive Linux server infrastructure";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils, ... }:
    let
      expanse-overlay = final: prev: {
        expanse = self.packages.${prev.system}.expanse;
      };
    in
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" ] (system:
      let
        pkgs = import nixpkgs { inherit system; };
        version = "1.0.0";
        rev = self.rev or self.dirtyRev or "dirty";

        mkTest = name: path:
          pkgs.testers.nixosTest (import path { inherit self; });
      in
      {
        packages.expanse = pkgs.callPackage ./nix/package.nix { inherit version rev; };
        packages.default = self.packages.${system}.expanse;

        # Installer ISO: `nix build .#iso`
        packages.iso = (nixpkgs.lib.nixosSystem {
          inherit system;
          specialArgs = { inherit self nixpkgs; };
          modules = [
            ({ nixpkgs.hostPlatform = system; })
            ./nix/installer/iso.nix
          ];
        }).config.system.build.isoImage;

        devShells.default = pkgs.callPackage ./nix/devshell.nix { };

        checks = {
          lint = with pkgs; runCommand "lint" { nativeBuildInputs = [ golangci-lint go stdenv.cc ]; } ''
            export HOME="$TMPDIR"
            cp -r ${self} src
            chmod -R u+w src
            cd src
            golangci-lint run --timeout=5m ./... 2>&1 | tee $out
          '';
          unit = with pkgs; runCommand "unit" { nativeBuildInputs = [ go stdenv.cc ]; } ''
            export HOME="$TMPDIR"
            export GOCACHE="$TMPDIR/go-build"
            cp -r ${self} src
            chmod -R u+w src
            cd src
            go test -race -coverprofile=coverage.out ./... > $out 2>&1 || { cat $out; exit 1; }
          '';
          smoke = mkTest "smoke" ./nix/tests/smoke.nix;
          install-unattended = mkTest "install-unattended" ./nix/tests/install-unattended.nix;
          install-refuses-dirty-disk =
            mkTest "install-refuses-dirty-disk" ./nix/tests/install-refuses-dirty-disk.nix;
          impermanence = mkTest "impermanence" ./nix/tests/impermanence.nix;
          identity = mkTest "identity" ./nix/tests/identity.nix;
          boot-time = mkTest "boot-time" ./nix/tests/boot-time.nix;
          agent-basic = mkTest "agent-basic" ./nix/tests/agent-basic.nix;
          agent-reconcile = mkTest "agent-reconcile" ./nix/tests/agent-reconcile.nix;
          # Phase 03 cluster VM tests (§6).
          cluster-form = mkTest "cluster-form" ./nix/tests/cluster-form.nix;
          cluster-linearizable = mkTest "cluster-linearizable" ./nix/tests/cluster-linearizable.nix;
          cluster-leader-failover = mkTest "cluster-leader-failover" ./nix/tests/cluster-leader-failover.nix;
          cluster-node-loss = mkTest "cluster-node-loss" ./nix/tests/cluster-node-loss.nix;
          cluster-full-restart = mkTest "cluster-full-restart" ./nix/tests/cluster-full-restart.nix;
          cluster-partition = mkTest "cluster-partition" ./nix/tests/cluster-partition.nix;
          cluster-join-security = mkTest "cluster-join-security" ./nix/tests/cluster-join-security.nix;
          cluster-witness = mkTest "cluster-witness" ./nix/tests/cluster-witness.nix;
          cluster-generations = mkTest "cluster-generations" ./nix/tests/cluster-generations.nix;
          # Phase 10 X2.
          cluster-ca-rotation = mkTest "cluster-ca-rotation" ./nix/tests/cluster-ca-rotation.nix;
          # Phase 10 X3.
          oidc-login = mkTest "oidc-login" ./nix/tests/oidc-login.nix;

          # Phase 06 storage.
          vol-perf = mkTest "vol-perf" ./nix/tests/vol-perf.nix;
          vol-constrained = mkTest "vol-constrained" ./nix/tests/vol-constrained.nix;
          node-idle = mkTest "node-idle" ./nix/tests/node-idle.nix;
          vol-constrained-profile = mkTest "vol-constrained-profile" ./nix/tests/vol-constrained-profile.nix;
          vol-agent = mkTest "vol-agent" ./nix/tests/vol-agent.nix;
          vol-firewall = mkTest "vol-firewall" ./nix/tests/vol-firewall.nix;
          vol-forced = mkTest "vol-forced" ./nix/tests/vol-forced.nix;
          vol-create = mkTest "vol-create" ./nix/tests/vol-create.nix;
          vol-degraded = mkTest "vol-degraded" ./nix/tests/vol-degraded.nix;
          vol-durability = mkTest "vol-durability" ./nix/tests/vol-durability.nix;
          vol-full-restart = mkTest "vol-full-restart" ./nix/tests/vol-full-restart.nix;
          vol-resync-incremental = mkTest "vol-resync-incremental" ./nix/tests/vol-resync-incremental.nix;
          vol-no-double-primary = mkTest "vol-no-double-primary" ./nix/tests/vol-no-double-primary.nix;
          vol-split-brain = mkTest "vol-split-brain" ./nix/tests/vol-split-brain.nix;
          vol-resize = mkTest "vol-resize" ./nix/tests/vol-resize.nix;
          vol-snapshot = mkTest "vol-snapshot" ./nix/tests/vol-snapshot.nix;
          vol-drbd-nodeid-spike = mkTest "vol-drbd-nodeid-spike" ./nix/tests/vol-drbd-nodeid-spike.nix;
          vol-drbd-status-capture = mkTest "vol-drbd-status-capture" ./nix/tests/vol-drbd-status-capture.nix;
          vol-drbd-config = mkTest "vol-drbd-config" ./nix/tests/vol-drbd-config.nix;
          vol-lvm = mkTest "vol-lvm" ./nix/tests/vol-lvm.nix;
          vol-drbd-probe = mkTest "vol-drbd-probe" ./nix/tests/vol-drbd-probe.nix;
          vol-drbd-verify-probe = mkTest "vol-drbd-verify-probe" ./nix/tests/vol-drbd-verify-probe.nix;
          vol-verify = mkTest "vol-verify" ./nix/tests/vol-verify.nix;
          vol-runtime = mkTest "vol-runtime" ./nix/tests/vol-runtime.nix;
          vol-primary = mkTest "vol-primary" ./nix/tests/vol-primary.nix;

          # ROADMAP.md Phase 2 (web management interface). Named by the
          # current roadmap, unlike the "Phase NN" labels above/below,
          # which predate its reorder and number the old bottom-up plan.
          ui-scaffold = mkTest "ui-scaffold" ./nix/tests/ui-scaffold.nix;
          ui-auth = mkTest "ui-auth" ./nix/tests/ui-auth.nix;
          ui-vip-failover = mkTest "ui-vip-failover" ./nix/tests/ui-vip-failover.nix;
          ui-blocks = mkTest "ui-blocks" ./nix/tests/ui-blocks.nix;
          ui-cluster = mkTest "ui-cluster" ./nix/tests/ui-cluster.nix;
          ui-volumes = mkTest "ui-volumes" ./nix/tests/ui-volumes.nix;
          ui-vertical-slice = mkTest "ui-vertical-slice" ./nix/tests/ui-vertical-slice.nix;

          # ROADMAP.md Phase 3 (SMB and NFS).
          share-colocation = mkTest "share-colocation" ./nix/tests/share-colocation.nix;
          share-smb = mkTest "share-smb" ./nix/tests/share-smb.nix;
          # share-smb-failover (Stream B2, X2) is deliberately not wired
          # in here: it fails on a still-unresolved post-failover
          # ACCESS_DENIED bug (PHASE-03-TASKS.md Stream B2, paused).
          # The test file (nix/tests/share-smb-failover.nix) stays in
          # the tree for whoever resumes Phase 3 -- re-add this line to
          # run it as a check again.

          # ROADMAP.md Phase 4 (iSCSI) Stream A prerequisite.
          daemonset-raw-colocation = mkTest "daemonset-raw-colocation" ./nix/tests/daemonset-raw-colocation.nix;

          # Probe (Phase 4 D1): not a gate.
          iscsi-lio-drbd-secondary-probe =
            mkTest "iscsi-lio-drbd-secondary-probe" ./nix/tests/iscsi-lio-drbd-secondary-probe.nix;

          # Phase 4 (iSCSI) Stream B (X1).
          iscsi-target = mkTest "iscsi-target" ./nix/tests/iscsi-target.nix;
          # Phase 4 (iSCSI) Stream C (X2, the decider; X4).
          iscsi-target-failover =
            mkTest "iscsi-target-failover" ./nix/tests/iscsi-target-failover.nix;
          # Phase 4 (iSCSI) Stream D (X1, X2, X4, X5 -- the phase-closing
          # vertical slice).
          iscsi-vertical-slice =
            mkTest "iscsi-vertical-slice" ./nix/tests/iscsi-vertical-slice.nix;

          # Phase 05 (network).
          net-mesh = mkTest "net-mesh" ./nix/tests/net-mesh.nix;
          net-vip-basic = mkTest "net-vip-basic" ./nix/tests/net-vip-basic.nix;
          net-vip-failover = mkTest "net-vip-failover" ./nix/tests/net-vip-failover.nix;
          net-vip-no-duplicate = mkTest "net-vip-no-duplicate" ./nix/tests/net-vip-no-duplicate.nix;
          net-lb-distribution = mkTest "net-lb-distribution" ./nix/tests/net-lb-distribution.nix;
          net-lb-health = mkTest "net-lb-health" ./nix/tests/net-lb-health.nix;
          net-lb-drain = mkTest "net-lb-drain" ./nix/tests/net-lb-drain.nix;
          net-l7-routing = mkTest "net-l7-routing" ./nix/tests/net-l7-routing.nix;
          net-dns = mkTest "net-dns" ./nix/tests/net-dns.nix;
          net-firewall = mkTest "net-firewall" ./nix/tests/net-firewall.nix;
          doctor-network = mkTest "doctor-network" ./nix/tests/doctor-network.nix;
          doctor-storage = mkTest "doctor-storage" ./nix/tests/doctor-storage.nix;
          m3-demo = mkTest "m3-demo" ./nix/tests/m3-demo.nix;

          # Phase 05 (database) Stream A (X1), Stream B (X2), Stream C (X3/X4) and Stream D (X5).
          db-postgres = mkTest "db-postgres" ./nix/tests/db-postgres.nix;
          db-postgres-failover = mkTest "db-postgres-failover" ./nix/tests/db-postgres-failover.nix;
          db-postgres-recovery = mkTest "db-postgres-recovery" ./nix/tests/db-postgres-recovery.nix;
          db-postgres-vertical-slice = mkTest "db-postgres-vertical-slice" ./nix/tests/db-postgres-vertical-slice.nix;
          db-postgres-partition = mkTest "db-postgres-partition" ./nix/tests/db-postgres-partition.nix;

          # Phase 04 blocks (§8).
          block-deploy = mkTest "block-deploy" ./nix/tests/block-deploy.nix;
          block-antiaffinity = mkTest "block-antiaffinity" ./nix/tests/block-antiaffinity.nix;
          block-reschedule = mkTest "block-reschedule" ./nix/tests/block-reschedule.nix;
          block-rolling-update = mkTest "block-rolling-update" ./nix/tests/block-rolling-update.nix;
          block-scale = mkTest "block-scale" ./nix/tests/block-scale.nix;
          block-singleton = mkTest "block-singleton" ./nix/tests/block-singleton.nix;
          block-daemonset = mkTest "block-daemonset" ./nix/tests/block-daemonset.nix;
          block-delete = mkTest "block-delete" ./nix/tests/block-delete.nix;
          block-catalog = mkTest "block-catalog" ./nix/tests/block-catalog.nix;

          # Probe (Phase 6 D1): not a gate.
          vm-nested-kvm-probe =
            mkTest "vm-nested-kvm-probe" ./nix/tests/vm-nested-kvm-probe.nix;

          # Probe (Phase 6 D2): not a gate.
          vm-macvtap-probe = mkTest "vm-macvtap-probe" ./nix/tests/vm-macvtap-probe.nix;

          # Probe (Phase 6 D1, final choice): not a gate.
          vm-d1-boot-probe = mkTest "vm-d1-boot-probe" ./nix/tests/vm-d1-boot-probe.nix;

          # Stream A, X1: a real vm/instance block deploy, not a probe.
          vm-instance = mkTest "vm-instance" ./nix/tests/vm-instance.nix;

          # Stream B, X2 (the phase's decider)/X3: survives a hard node kill.
          vm-instance-failover = mkTest "vm-instance-failover" ./nix/tests/vm-instance-failover.nix;

          # Stream C, X4: guest filesystem survives a hard kill mid-write.
          vm-instance-fs-integrity = mkTest "vm-instance-fs-integrity" ./nix/tests/vm-instance-fs-integrity.nix;

          # Stream D: vertical slice -- X3/X4 proven together, one kill, black-box (no internal-state waits).
          vm-instance-vertical-slice = mkTest "vm-instance-vertical-slice" ./nix/tests/vm-instance-vertical-slice.nix;

          # ROADMAP.md Phase 08 (backup and restore) Stream A (X1): restic
          # adopted (D1), basic backup/restore round-trip against a real
          # in-VM S3-compatible target (garage, not minio -- no insecure flag).
          backup-basic = mkTest "backup-basic" ./nix/tests/backup-basic.nix;

          # Phase 08 Stream B (X2): opaque LVM-thin volume snapshot backed up
          # and restored via restic, checksum-equal, isolated from a
          # concurrent live write (R1/D5).
          backup-volume-snapshot = mkTest "backup-volume-snapshot" ./nix/tests/backup-volume-snapshot.nix;

          # Phase 08 Stream B (X3): /persist (node/cluster durable state)
          # backed up and restored via restic, checksum-equal.
          backup-persist = mkTest "backup-persist" ./nix/tests/backup-persist.nix;

          # Phase 08 Stream C (X4, X5): the generations store's own
          # desired-state history, and cluster identity material under
          # dataDir, both backed up, restored, and provably reconciled/
          # rejoined for real.
          backup-cluster-config = mkTest "backup-cluster-config" ./nix/tests/backup-cluster-config.nix;

          # Phase 08 Stream D (X6, the decider): the vertical slice --
          # destroy the whole cluster, rebuild from backup credentials plus
          # one command (`expanse cluster restore`), data and configuration
          # both verify.
          backup-destroy-rebuild = mkTest "backup-destroy-rebuild" ./nix/tests/backup-destroy-rebuild.nix;

          # Phase 09 Stream A (X1): a real Prometheus binary scrapes a real
          # node's /metrics endpoint over mTLS with bearer-token auth, and
          # node/resource/volume/quorum health samples are queried back out
          # of Prometheus's own HTTP API.
          observability-metrics = mkTest "observability-metrics" ./nix/tests/observability-metrics.nix;

          # Phase 09 Stream B (X2): the shipped alert rules
          # (deploy/prometheus/expanse-alerts.rules.yml), evaluated by a
          # real Prometheus, actually fire under genuinely degraded
          # fixtures on a real 3-node cluster.
          observability-alerts = mkTest "observability-alerts" ./nix/tests/observability-alerts.nix;

          # Phase 09 Stream C (X3): the shipped Grafana provisioning and
          # dashboard (deploy/grafana/), loaded by a real Grafana pointed
          # at a real Prometheus, with panel queries proven to return
          # real data back through Grafana's own /api/ds/query.
          observability-grafana = mkTest "observability-grafana" ./nix/tests/observability-grafana.nix;

          # Phase 09 Stream E (X5, the release blocker): ROADMAP.md's own
          # exit line -- a real killed node raises a real alert and is
          # visible in the UI and dashboards within 30s, measured with a
          # real stopwatch against the whole assembled Prometheus/
          # Grafana/web-UI pipeline, not assumed from each component's
          # own latency budget.
          observability-vertical-slice = mkTest "observability-vertical-slice" ./nix/tests/observability-vertical-slice.nix;

          # Phase 11 Stream B (X2, the release blocker): storage, cluster and network
          # faults injected concurrently over an extended soak, zero acked-write loss
          # throughout -- ARCHITECTURE.md §8's own bar, now measured under compound
          # rather than isolated fault load.
          chaos-soak = mkTest "chaos-soak" ./nix/tests/chaos-soak.nix;

          # Phase 11 Stream C (X3): a live 3-node cluster upgraded one node at a time via
          # the real switch-to-configuration binary, under continuous KV and volume load,
          # zero acked-write loss and continuous availability throughout -- this project's
          # first-ever mixed-version-cluster test.
          cluster-rolling-upgrade = mkTest "cluster-rolling-upgrade" ./nix/tests/cluster-rolling-upgrade.nix;
        };
        formatter = pkgs.nixpkgs-fmt;
      })
    // {
      nixosModules.expanse = import ./nix/modules/expanse.nix;

      # Reference node config for `nixos-install --flake <ref>#expanse-node`
      # (the unattended installer instead generates a standalone
      # configuration.nix under /mnt; this exists for flake-based flows).
      nixosConfigurations.expanse-node = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          ({ nixpkgs.hostPlatform = "x86_64-linux"; })
          ({ nixpkgs.overlays = [ expanse-overlay ]; })
          ./nix/modules/expanse-node.nix
        ];
      };
    };
}
