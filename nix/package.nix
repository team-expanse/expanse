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
  meta = {
    mainProgram = "expanse";
  };
}