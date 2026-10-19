# Phase 14 F: a VM deployed from the web form, from a cloud image served over HTTP, comes up with
# the cloud-init user, key and hostname. The scenario is python/ui_vm_cloud_image.py.
{ self }:
{ pkgs, lib, ... }:
let
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "ui-vm-cloud-image-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/ui_forms_common.py} ${./python/ui_vm_cloud_image.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
  keys = import "${pkgs.path}/nixos/tests/ssh-keys.nix" pkgs;
  image = import ./cloud-image.nix { nixpkgs = self.inputs.nixpkgs; system = pkgs.stdenv.hostPlatform.system; };
in
{
  name = "expanse-ui-vm-cloud-image";

  nodes.n1 = { ... }: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
      ./curl-cookie-jar.nix
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
    environment.systemPackages = with pkgs; [ curl qemu-utils ];
    boot.kernelModules = [ "kvm" ];
    virtualisation.memorySize = 4096;
    virtualisation.cores = 4;
    # The image cache and its raw conversion live on the root disk.
    virtualisation.diskSize = 8192;
  };

  # The LAN: serves the image and its SHA256SUMS over HTTP, hands out addresses by DHCP, and
  # logs in to the guest (the node holding a macvtap guest cannot reach it).
  nodes.client = { ... }: {
    environment.systemPackages = [ pkgs.openssh pkgs.python3 ];
    environment.etc."ssh-key" = { source = keys.snakeOilPrivateKey; mode = "0600"; };
    networking.firewall.enable = false;
    services.dnsmasq = {
      enable = true;
      settings = {
        port = 0;
        interface = "eth1";
        bind-interfaces = true;
        dhcp-range = "192.168.1.100,192.168.1.150,255.255.255.0,1h";
      };
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${shim}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/single_node_common.py}
    ${builtins.readFile ./python/ui_forms_common.py}
    IMAGE = "${image}/nixos.qcow2"
    PUBKEY = "${lib.strings.trim keys.snakeOilPublicKey}"
    ${builtins.readFile ./python/ui_vm_cloud_image.py}
  '';
}
