# Phase 11 X3: a rolling upgrade of a live 3-node cluster, one node at a time, under
# continuous load, with zero acked-write loss and continuous read/write availability
# throughout -- this project's first-ever mixed-version-cluster test (D3, ARCHITECTURE.md
# A44). Scenario: python/rolling_upgrade_main.py; see its docstring for how the two
# continuous-load legs (KV/control-plane, volume/data-plane) are kept meaningful across a
# real switch-to-configuration.
#
# Every node boots a pinned older `expanse` build (oldRev below) and also carries a
# `specialisation.upgraded` built from the current source tree (self.packages.expanse) --
# switched to one node at a time via the real switch-to-configuration binary, the same one
# internal/agent/nix.ExecDriver.Switch shells out to in production.
{ self }:
{ pkgs, lib, ... }:
let
  # The v1.0.0 release tag: every release since is checked as an upgrade from it (Phase 12 R2).
  oldRev = "f2e4a02012f77c54f2838d2905a53ee09f4e89e9";
  # "." (relative to the invoking shell's cwd, i.e. the repo root) rather than
  # `toString ../..`: toString on a Nix path value copies it into the store first (losing
  # .git), which fetchGit then can't clone from. Nix warns this relative form is slated
  # for removal (github.com/NixOS/nix/issues/12281); it is pinned by `rev`, so the fetch
  # itself is still pure/reproducible -- only the *locating* of the source is relative.
  oldSrc = builtins.fetchGit {
    url = ".";
    rev = oldRev;
  };
  mkExpanse = src: rev: pkgs.buildGoModule {
    pname = "expanse";
    version = rev;
    inherit src;
    vendorHash = null; # deps are vendored in ./vendor, same as nix/package.nix
    env.CGO_ENABLED = "0";
    ldflags = [ "-s" "-w" "-X main.buildVersion=${rev}" "-X main.buildCommit=${rev}" ];
    subPackages = [ "cmd/expanse" "cmd/expanse-block-run" ];
    meta.mainProgram = "expanse";
  };
  expanseOld = mkExpanse oldSrc oldRev;
  expanseNew = self.packages.${pkgs.system}.expanse;

  lint = pkgs.runCommand "cluster-rolling-upgrade-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/vol_cluster.py} ${./python/rolling_upgrade_main.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  # The recorder runs on the VMs as a standalone executable (identical to vol-durability.nix's).
  recorder = pkgs.runCommand "cluster-rolling-upgrade-rec" { } ''
    { echo "#!${pkgs.python3}/bin/python3"; cat ${./python/vol_durability_rec.py}; } > $out
    chmod +x $out
  '';
  nodeCommon = idx: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
    ];
    nixpkgs.overlays = [
      (final: prev: { expanse = expanseOld; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    expanse.storage-test.enable = true;
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    networking.firewall.allowedTCPPorts = [ 9440 ];
    virtualisation.memorySize = 2048;
    # The node's own next generation: current HEAD's expanse, nothing else -- a
    # software-only diff, pre-built into the same closure exactly like a real
    # nixos-rebuild boot preparing the next generation ahead of switch time.
    specialisation.upgraded.configuration = {
      nixpkgs.overlays = [
        (final: prev: { expanse = expanseNew; })
      ];
    };
  };
in
{
  name = "expanse-cluster-rolling-upgrade";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    REC = "/root/cluster-rolling-upgrade-rec"
    for m in [n1, n2, n3]:
        m.copy_from_host("${recorder}", REC)
    ${builtins.readFile ./python/rolling_upgrade_main.py}
  '';
}
