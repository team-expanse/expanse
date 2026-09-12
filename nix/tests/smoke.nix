{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-smoke";

  nodes.machine = { config, pkgs, ... }: {
    environment.systemPackages = [ expanse ];
    virtualisation.memorySize = 1024;
    virtualisation.cores = 2;
  };

  testScript = ''
    machine.wait_for_unit("multi-user.target")
    out = machine.succeed("expanse version")
    assert "expanse" in out, f"unexpected version output: {out}"
    machine.succeed("expanse agent --help")
    machine.succeed("expanse ctl --help")
  '';
}