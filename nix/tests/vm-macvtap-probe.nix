# Probe (Phase 6 D2): does macvtap bridging on the node's one real
# network interface (eth1 in this harness -- eth0 is the test driver's
# own backdoor channel, not a second application-usable NIC; see
# ARCHITECTURE.md A34) give a VM guest a LAN-reachable identity without
# disrupting the host's own existing traffic on that same interface
# (R2), and is the well-known "host can't reach its own macvtap child"
# limitation real here, with a working sibling-macvtap workaround?
# Observations only; nothing is asserted -- same role as
# iscsi-lio-drbd-secondary-probe.nix played for Phase 4 D1.
{ self }:
{ pkgs, lib, ... }:
{
  name = "expanse-vm-macvtap-probe";
  nodes = {
    n1 = { ... }: { virtualisation.memorySize = 768; };
    n2 = { ... }: { virtualisation.memorySize = 512; };
  };
  testScript = builtins.readFile ./vm-macvtap-probe.py;
}
