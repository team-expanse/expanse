{ lib, buildGoModule, version, rev }:
buildGoModule {
  pname = "expanse";
  inherit version;
  src = lib.cleanSource ../.;
  vendorHash = "sha256-komX1AmHt2NoF1x6xsNa2RFkfVzOXfYEMPhT0zwMxjw=";
  CGO_ENABLED = "0";
  ldflags = [
    "-s" "-w"
    "-X main.buildVersion=${version}"
    "-X main.buildCommit=${rev}"
  ];
  meta = {
    mainProgram = "expanse";
  };
}