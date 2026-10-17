# Phase 13 D1: a vm/instance larger than the scheduler's old fixed node capacity (4 CPU, 8 GiB,
# 100 GiB) on every axis, booting the dev guest. The scenario is python/vm_instance_large.py.
{ self }:
{ pkgs, lib, ... }:
let
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "vm-instance-large-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/vm_instance_large.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';

  guestIP = "192.168.1.61";
  keys = import "${pkgs.path}/nixos/tests/ssh-keys.nix" pkgs;

  # The shipped dev guest with a static address (the test network has no DHCP) and a test key.
  image = import ../guests/dev/image.nix {
    nixpkgs = self.inputs.nixpkgs;
    system = pkgs.stdenv.hostPlatform.system;
    modules = [
      {
        networking.usePredictableInterfaceNames = false;
        networking.useDHCP = false;
        networking.interfaces.eth0.ipv4.addresses = [{ address = guestIP; prefixLength = 24; }];
        expanse.devGuest.authorizedKeys = [ keys.snakeOilPublicKey ];
      }
    ];
  };
in
{
  name = "expanse-vm-instance-large";

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
    # Sparse qcow2: only what the guest writes takes host disk.
    expanse.storage-test.diskSizeMB = 300000;
    expanse.agent.raftAdvertise = "192.168.1.1:7444";
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalInterface = "eth1";
    environment.systemPackages = [ pkgs.qemu_kvm ];
    boot.kernelModules = [ "kvm" ];
    virtualisation.memorySize = 49152;
    virtualisation.cores = 16;
  };

  # A workstation on the LAN; the host holding a macvtap guest cannot reach it itself.
  nodes.client = { ... }: {
    environment.systemPackages = [ pkgs.openssh ];
    environment.etc."ssh-key" = { source = keys.snakeOilPrivateKey; mode = "0600"; };
    programs.ssh.extraConfig = "IdentityFile /etc/ssh-key";
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
    ${builtins.readFile ./python/vm_instance_large.py}
  '';
}
