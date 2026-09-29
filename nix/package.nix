{ lib, buildGoModule, version, rev }:
buildGoModule {
  pname = "expanse";
  inherit version;
  src = lib.cleanSource ../.;
  vendorHash = null; # deps are vendored in ./vendor
  env.CGO_ENABLED = "0";
  ldflags = [
    "-s" "-w"
    "-X github.com/expanse/expanse/internal/version.Version=${version}"
    "-X github.com/expanse/expanse/internal/version.Commit=${rev}"
  ];
  # The block runtime helper (Phase 04) builds alongside expanse and is
  # installed next to it at /run/current-system/sw/bin.
  subPackages = [ "cmd/expanse" "cmd/expanse-block-run" ];

  meta = {
    license = lib.licenses.asl20;
    mainProgram = "expanse";
  };
}