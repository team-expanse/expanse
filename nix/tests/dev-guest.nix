# Phase 13 C4/E2: docs/DEVELOPING.md's dev guest on a vm/instance with cpuModel host, reached over
# SSH from a second machine that also holds its restic backups. The scenario is python/dev_guest.py.
{ self }:
{ pkgs, lib, ... }:
let
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "dev-guest-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/dev_guest.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';

  guestIP = "192.168.1.60";
  # The client is node 1 by name order; it runs the restic REST server the guest backs up to.
  backupURL = "rest:http://192.168.1.1:8000/dev";
  keys = import "${pkgs.path}/nixos/tests/ssh-keys.nix" pkgs;

  # The shipped guest with a static address (the test network has no DHCP), a test key and the
  # L3 check, whose kernel is the guest's own.
  image = import ../guests/dev/image.nix {
    nixpkgs = self.inputs.nixpkgs;
    system = pkgs.stdenv.hostPlatform.system;
    modules = [
      ({ pkgs, config, ... }: {
        networking.usePredictableInterfaceNames = false;
        networking.useDHCP = false;
        networking.interfaces.eth0.ipv4.addresses = [{ address = guestIP; prefixLength = 24; }];
        expanse.devGuest.authorizedKeys = [ keys.snakeOilPublicKey ];
        expanse.devGuest.backup = {
          enable = true;
          repository = backupURL;
          passwordFile = "/etc/restic-test-password";
        };
        environment.etc."restic-test-password".text = "test-only-password";
        environment.etc."dev-guest-test/l3-kernel".source =
          "${config.boot.kernelPackages.kernel}/${config.system.boot.loader.kernelFile}";
        expanse.devGuest.extraPackages = [
          (pkgs.writeShellApplication {
            name = "dev-guest-l3-check";
            runtimeInputs = [ pkgs.qemu_kvm pkgs.coreutils pkgs.gnugrep ];
            text = builtins.readFile ./dev-guest-l3.sh;
          })
        ];
      })
    ];
  };
in
{
  name = "expanse-dev-guest";

  nodes.n1 = { ... }: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
    ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "00000001";
    expanse.hostname = "n1";
    expanse.storage-test.enable = true;
    expanse.storage-test.diskSizeMB = 16384;
    expanse.agent.raftAdvertise = "192.168.1.1:7444";
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalInterface = "eth1";
    environment.systemPackages = [ pkgs.qemu_kvm ];
    boot.kernelModules = [ "kvm" ];
    virtualisation.memorySize = 5120;
    virtualisation.cores = 4;
  };

  # A workstation on the LAN; the host holding a macvtap guest cannot reach it itself.
  nodes.client = { ... }: {
    environment.systemPackages = [ pkgs.openssh ];
    environment.etc."ssh-key" = { source = keys.snakeOilPrivateKey; mode = "0600"; };
    programs.ssh.extraConfig = "IdentityFile /etc/ssh-key";
    services.restic.server = { enable = true; listenAddress = "8000"; extraFlags = [ "--no-auth" ]; };
    networking.firewall.allowedTCPPorts = [ 8000 ];
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${shim}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/single_node_common.py}
    IMAGE = "${image}/nixos.img"
    GUEST_IP = "${guestIP}"
    ${builtins.readFile ./python/dev_guest.py}
  '';
}
