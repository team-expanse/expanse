# The "newqemu" specialisation stands in for a release with another QEMU: its vm/instance runtime
# carries a copy of the same qemu-kvm binary at a new store path. qemu finds its firmware beside
# the binary (../share/qemu), so share/ comes along.
{ pkgs, lib, ... }:
let
  runtime = (import ../block-runtimes.nix { inherit pkgs; })."vm/instance";
  qemuNext = pkgs.runCommand "qemu-kvm-next" { } ''
    mkdir -p $out/bin
    cp -L ${runtime}/bin/qemu-kvm $out/bin/qemu-kvm
    ln -s ${runtime}/share $out/share
  '';
in
{
  specialisation.newqemu.configuration.expanse.agent.blockRuntimes."vm/instance" = lib.mkForce (pkgs.buildEnv {
    name = "expanse-runtime-vm-instance-next";
    paths = [ (lib.hiPrio qemuNext) runtime ];
  });
}
