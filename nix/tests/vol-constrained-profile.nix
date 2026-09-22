# R9 investigation (E7 follow-up): vol-constrained measured expansed at 25-27% of one core
# idle, against a 3% budget, with `top -H` showing the cost spread across ~6 threads and no
# single hot loop — inconclusive without a real CPU profile. This is that profile: the same
# 4 GB/2-core/3-node scenario, but the primary's idle window is captured with net/http/pprof
# (--pprof-addr, loopback only) instead of `top -H`, and `go tool pprof` is run against it
# inside the VM. Diagnostic, not a budget gate — it prints, it does not assert a threshold.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "vol-constrained-profile-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    cat ${./cluster-common.py} vol_cluster.py vol_constrained_profile.py \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
  nodeCommon = idx: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
    ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    environment.systemPackages = [ pkgs.go pkgs.curl ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    expanse.storage-test.enable = true;
    # Same advertised-minimum sizing as vol-constrained (E7) — the profile has to be of the
    # same conditions that measured the overage, not a looser stand-in.
    expanse.storage-test.diskSizeMB = 4096;
    expanse.storage-test.poolPercent = 80;
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    expanse.agent.pprofAddr = "127.0.0.1:6060";
    virtualisation.memorySize = 4096;
    virtualisation.cores = 2;
  };
in
{
  name = "expanse-vol-constrained-profile";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    EXPANSE_BIN = "${self.packages.${pkgs.system}.expanse}/bin/expanse"
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/vol_constrained_profile.py}
  '';
}
