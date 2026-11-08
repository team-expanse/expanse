# A rollout whose release needs a reboot (it changes the initrd), with "Reboot nodes that need it"
# on: each node is drained, reboots itself through a real boot loader into the release, and goes
# back in service. `size` nodes; the scenario is `script`.
{ self, name, size, script }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
  # A one-node cluster names n1 only; vol_cluster.py's NODES names n1..n3.
  shim = lib.optionalString (size == 1) "n2 = n3 = n1\n";
  commons = [ ./cluster-common.py ./block-common.py ];
  helpers = [ ./python/vol_cluster.py ./python/single_node_common.py ./python/ui_forms_common.py ./python/rolling_reboot_common.py script ];
  lint = pkgs.runCommand "${name}-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${lib.concatStringsSep " " commons}; printf '%s' ${lib.escapeShellArg shim}; cat ${lib.concatStringsSep " " helpers}; } \
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

  node = idx: {
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
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.releaseURL = "http://rel:8000";
    expanse.agent.releaseKey = lib.fileContents ./fixtures/release-test-key.pub;
    environment.systemPackages = with pkgs; [ curl ];
    virtualisation.memorySize = 2048;
    # A real boot loader, so a reboot boots the generation the upgrade made the default.
    virtualisation.useBootLoader = true;
    virtualisation.useEFIBoot = true;
    specialisation.upgraded.configuration = {
      nixpkgs.overlays = [ (final: prev: { expanse = prev.expanse.override { version = "99.0.0"; }; }) ];
      # A new initrd: the upgrade needs a reboot, without building a kernel.
      boot.initrd.systemd.contents."/etc/expanse-test-release".text = "99.0.0\n";
    };
  };
in
{
  inherit name;

  nodes = lib.genAttrs (map (i: "n${toString i}") (lib.range 1 size)) (n: { ... }: node (lib.toInt (lib.removePrefix "n" n))) // {
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
    ${lib.concatMapStrings builtins.readFile commons}
    ${shim}
    ${lib.concatMapStrings builtins.readFile helpers}
  '';
}
