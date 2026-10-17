# Phase 13 B1: a node with four NICs, as on the R630, where only eno3 reaches a DHCP server.
# eno1 has a link but no server; eno2 and eno4 have no carrier. The scenario is python/net_multi_nic.py.
{ self }:
{ pkgs, lib, ... }:
let
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "net-multi-nic-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/net_multi_nic.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';

  # A netboot guest; its NetworkManager takes an address by DHCP over the block's macvtap.
  l2 = import "${pkgs.path}/nixos" {
    system = pkgs.system;
    configuration = {
      imports = [ "${pkgs.path}/nixos/modules/installer/netboot/netboot-minimal.nix" ];
      netboot.squashfsCompression = "gzip -Xcompression-level 1";
      documentation.enable = lib.mkForce false;
      boot.kernelParams = [ "console=ttyS0" "panic=-1" ];
    };
  };
  kernel = "${l2.config.system.build.kernel}/${l2.config.system.boot.loader.kernelFile}";
  initrd = "${l2.config.system.build.netbootRamdisk}/initrd";
  cmdline = "init=${l2.config.system.build.toplevel}/init ${toString l2.config.boot.kernelParams}";
in
{
  name = "expanse-net-multi-nic";

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
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    # No externalInterface or raftAdvertise: the installed default (auto) is what is under test.
    expanse.agent.externalVIPPool = "192.168.3.200-192.168.3.200";
    # Four unaddressed NICs and no slirp eth0, so DHCP alone decides the default route.
    virtualisation.vlans = lib.mkForce [ ];
    virtualisation.interfaces = lib.genAttrs [ "eno1" "eno2" "eno3" "eno4" ] (n: {
      vlan = lib.toInt (lib.removePrefix "eno" n);
      assignIP = false;
    });
    virtualisation.qemu.networkingOptions = lib.mkForce [ ];
    boot.kernelModules = [ "kvm" ];
    virtualisation.memorySize = 4608;
    virtualisation.cores = 2;
  };

  # The site's router on eno3's segment: DHCP with reservations for n1 and the guest.
  nodes.router = { ... }: {
    virtualisation.vlans = [ 3 ];
    networking.firewall.allowedUDPPorts = [ 67 ];
    environment.systemPackages = [ pkgs.curl ];
    services.dnsmasq = {
      enable = true;
      resolveLocalQueries = false;
      settings = {
        port = 0;
        interface = "eth1";
        dhcp-range = [ "192.168.3.100,192.168.3.150,1h" ];
        dhcp-option = [ "option:router,192.168.3.2" ];
        dhcp-host = [ "52:54:00:12:03:01,192.168.3.10" "52:54:00:aa:00:01,192.168.3.20" ];
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
    KERNEL = "${kernel}"
    INITRD = "${initrd}"
    CMDLINE = "${cmdline}"
    ${builtins.readFile ./python/net_multi_nic.py}
  '';
}
