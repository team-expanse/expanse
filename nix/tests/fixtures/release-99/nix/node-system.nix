# The rollout test's "release": a link to each node's prebuilt specialisation, so nothing compiles.
{ ... }:
let upgraded = builtins.storePath "/run/current-system/specialisation/upgraded";
in {
  config.system.build.toplevel = derivation {
    name = "nixos-system-upgraded";
    system = builtins.currentSystem;
    builder = "/bin/sh";
    # sh keeps the log open until ln exits: ln closes its output first, and Nix kills on EOF (NixOS/nix#2176).
    args = [ "-c" "${upgraded}/sw/bin/ln -s ${upgraded} $out; status=$?; exit $status" ];
  };
}
