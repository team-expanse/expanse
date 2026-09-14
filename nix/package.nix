{ lib, buildGoModule, version, rev }:
buildGoModule {
  pname = "expanse";
  inherit version;
  src = lib.cleanSource ../.;
  vendorHash = null; # deps are vendored in ./vendor
  env.CGO_ENABLED = "0";
  ldflags = [
    "-s" "-w"
    "-X main.buildVersion=${version}"
    "-X main.buildCommit=${rev}"
  ];
  # The block runtime helper (Phase 04) builds alongside expanse and is
  # installed next to it at /run/current-system/sw/bin.
  subPackages = [ "cmd/expanse" "cmd/expanse-block-run" ];

  meta = {
    mainProgram = "expanse";
  };
}