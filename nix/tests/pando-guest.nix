# docs/PANDO.md's recipe end to end: the Pando guest image written onto a pre-created volume,
# booted by a vm/instance block, and Pando's state surviving a crash of the node running it.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "pando-guest-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/pando_guest.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';

  guestIP = "192.168.1.60";
  adminPassword = "pando-test-admin-pw";

  # The shipped guest with a static address (the test network has no DHCP) and a known admin.
  image = import ../guests/pando/image.nix {
    nixpkgs = self.inputs.nixpkgs;
    system = pkgs.stdenv.hostPlatform.system;
    modules = [{
      networking.usePredictableInterfaceNames = false;
      networking.useDHCP = false;
      networking.interfaces.eth0.ipv4.addresses = [{ address = guestIP; prefixLength = 24; }];
      expanse.pandoGuest.settings.PANDO_ADMIN_PASSWORD = adminPassword;
    }];
  };

  nodeCommon = idx: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
    ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    expanse.storage-test.enable = true;
    expanse.storage-test.diskSizeMB = 16384;
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalInterface = "eth1";
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.qemu_kvm ];
    boot.kernelModules = [ "kvm" ];
    virtualisation.memorySize = 5120;
    virtualisation.cores = 2;
  };
in
{
  name = "expanse-pando-guest";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    IMAGE = "${image}/nixos.img"
    GUEST_IP = "${guestIP}"
    ADMIN_PW = "${adminPassword}"
    ${builtins.readFile ./python/pando_guest.py}
  '';
}
