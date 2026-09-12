{ pkgs, ... }:
let
  go = pkgs.go;
in
pkgs.mkShell {
  packages = [
    go
    pkgs.gopls
    pkgs.golangci-lint
    pkgs.gofumpt
    pkgs.delve
    pkgs.protobuf
    pkgs.protoc-gen-go
    pkgs.protoc-gen-go-grpc
    pkgs.qemu_kvm
    pkgs.nixos-rebuild
    pkgs.jq
    pkgs.just
    pkgs.gnumake
    pkgs.git
  ];

  shellHook = ''
    echo "=== Expanse DevShell ==="
    echo "go version: $(${go}/bin/go version)"
    echo "Run 'make help' to see available tasks."
  '';
}