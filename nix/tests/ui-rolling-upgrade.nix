# Phase 15 U2/U3: the Upgrades page rolls a 3-node cluster to a signed release served by `rel`.
# The scenario is python/ui_rolling_upgrade.py.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
  lint = pkgs.runCommand "ui-rolling-upgrade-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cat ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/ui_forms_common.py} ${./python/ui_rolling_upgrade.py} \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';

  # The release as release.sh publishes it: tarball, SHA256SUMS and its signature.
  release = pkgs.runCommand "expanse-test-release-99.0.0" { } ''
    mkdir -p src $out/v99.0.0
    cp -r ${./fixtures/release-99} src/expanse-99.0.0
    tar -C src -czf $out/v99.0.0/expanse-99.0.0.tar.gz expanse-99.0.0
    cd $out/v99.0.0
    sha256sum expanse-99.0.0.tar.gz > SHA256SUMS
    ${expanse}/bin/expanse upgrade sign --key ${./fixtures/release-test-key.sec} SHA256SUMS
  '';

  nodeCommon = idx: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
      ./curl-cookie-jar.nix
    ];
    nixpkgs.overlays = [ (final: prev: { expanse = expanse; }) ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    expanse.storage-test.enable = true;
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.releaseURL = "http://rel:8000";
    expanse.agent.releaseKey = lib.fileContents ./fixtures/release-test-key.pub;
    environment.systemPackages = with pkgs; [ curl ];
    virtualisation.memorySize = 2048;
    # Test VMs have no boot loader to install; switch-to-configuration switch would fail on it.
    system.build.installBootLoader = lib.mkForce "${pkgs.coreutils}/bin/true";
    specialisation.upgraded.configuration.nixpkgs.overlays = [
      (final: prev: { expanse = prev.expanse.override { version = "99.0.0"; }; })
    ];
  };
in
{
  name = "expanse-ui-rolling-upgrade";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    rel = { ... }: {
      networking.firewall.allowedTCPPorts = [ 8000 ];
      systemd.services.release-server = {
        wantedBy = [ "multi-user.target" ];
        serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 -m http.server 8000 --directory ${release}";
      };
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/ui_forms_common.py}
    ${builtins.readFile ./python/ui_rolling_upgrade.py}
  '';
}
