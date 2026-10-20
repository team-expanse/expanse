# The nixpkgs this source's flake.lock pins, for nodes that build without flakes. Same narHash as the
# flake input, so a node that already holds it needs no download.
let
  locked = (builtins.fromJSON (builtins.readFile ../flake.lock)).nodes.nixpkgs.locked;
in
builtins.fetchTarball {
  url = "https://github.com/${locked.owner}/${locked.repo}/archive/${locked.rev}.tar.gz";
  sha256 = locked.narHash;
}
