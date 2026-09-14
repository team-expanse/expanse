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
        version = "0.0.1";
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

          # Phase 04 blocks (§8).
          block-deploy = mkTest "block-deploy" ./nix/tests/block-deploy.nix;
          block-antiaffinity = mkTest "block-antiaffinity" ./nix/tests/block-antiaffinity.nix;
          block-reschedule = mkTest "block-reschedule" ./nix/tests/block-reschedule.nix;
          block-rolling-update = mkTest "block-rolling-update" ./nix/tests/block-rolling-update.nix;
          block-scale = mkTest "block-scale" ./nix/tests/block-scale.nix;
          block-singleton = mkTest "block-singleton" ./nix/tests/block-singleton.nix;
          block-daemonset = mkTest "block-daemonset" ./nix/tests/block-daemonset.nix;
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
