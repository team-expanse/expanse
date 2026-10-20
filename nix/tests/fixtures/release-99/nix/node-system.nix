# The rollout test's "release": a link to each node's prebuilt specialisation, so nothing compiles.
{ ... }:
let upgraded = builtins.storePath "/run/current-system/specialisation/upgraded";
in {
  config.system.build.toplevel = derivation {
    name = "nixos-system-upgraded";
    system = builtins.currentSystem;
    builder = "/bin/sh";
    args = [ "-c" "exec ${upgraded}/sw/bin/ln -s ${upgraded} $out" ];
  };
}
