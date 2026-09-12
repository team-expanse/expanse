{
  description = "Expanse — flexible, deterministic, expansive Linux server infrastructure";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-24.11";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils, ... }:
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" ] (system:
      let
        pkgs = import nixpkgs { inherit system; };
        version = "0.0.1";
        rev = self.rev or self.dirtyRev or "dirty";
      in {
        packages.expanse = pkgs.callPackage ./nix/package.nix { inherit version rev; };
        packages.default = self.packages.${system}.expanse;
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
          smoke = pkgs.nixosTest (import ./nix/tests/smoke.nix { inherit self; });
        };
        formatter = pkgs.nixpkgs-fmt;
      })
    // {
      nixosModules.expanse = import ./nix/modules/expanse.nix;
    };
}