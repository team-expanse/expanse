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
          lint = with pkgs; runCommand "lint" { nativeBuildInputs = [ golangci-lint ]; } ''
            cp -r ${self} src
            cd src
            golangci-lint run --timeout=5m ./... 2>&1 | tee $out
          '';
          unit = with pkgs; runCommand "unit" { nativeBuildInputs = [ go ]; } ''
            cp -r ${self} src
            cd src
            go test -race -coverprofile=coverage.out ./... > $out 2>&1
          '';
          smoke = mkTest "smoke" ./nix/tests/smoke.nix;
          install-unattended = mkTest "install-unattended" ./nix/tests/install-unattended.nix;
          install-refuses-dirty-disk =
            mkTest "install-refuses-dirty-disk" ./nix/tests/install-refuses-dirty-disk.nix;
          impermanence = mkTest "impermanence" ./nix/tests/impermanence.nix;
          identity = mkTest "identity" ./nix/tests/identity.nix;
          boot-time = mkTest "boot-time" ./nix/tests/boot-time.nix;
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
