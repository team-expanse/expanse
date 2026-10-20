# What a release's binary cache must hold: the installer's reference nodes and the grafana plugins,
# built from this source tree exactly as an upgraded node builds them. release.sh keeps only the
# paths cache.nixos.org lacks.
{ system ? builtins.currentSystem }:
let
  src = ../.;
  configuration = layout: builtins.toFile "configuration-${layout}.nix" (builtins.replaceStrings
    [ "./expanse/" "@rev@" "./hardware-configuration.nix" ]
    [ "${src}/" "release" "${src}/test/fixtures/install/hardware-configuration.nix" ]
    (builtins.readFile ../test/fixtures/install/configuration-${layout}.nix));
  node = layout: import ./node-system.nix { configuration = configuration layout; inherit system; };
  pkgs = (node "single").pkgs;
in
pkgs.linkFarm "expanse-release-cache-roots" [
  { name = "single"; path = (node "single").config.system.build.toplevel; }
  { name = "mirror"; path = (node "mirror").config.system.build.toplevel; }
  { name = "grafana-plugins"; path = pkgs.callPackage ./grafana-plugins.nix { }; }
]
