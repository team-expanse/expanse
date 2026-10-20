# The rollout test's "release": each node's prebuilt specialisation, so upgrading builds nothing.
{ ... }:
{ config.system.build.toplevel = builtins.storePath "/run/current-system/specialisation/upgraded"; }
