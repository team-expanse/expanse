# A cluster of nodeCount nodes running two VM workloads that serve HTTP from their replicated disks:
# measures forming, placement, and failover on node loss and on leader loss. A benchmark, not a gate.
# Each VM is pinned to coresPerVM physical cores.
{ self, nodeCount ? 6, coresPerVM ? 2 }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "cluster-vm-workloads-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/cluster_vm_workloads.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';

  netbootPath = "${pkgs.path}/nixos/modules/installer/netboot/netboot-minimal.nix";

  # One netboot guest for both workloads; its name and address come from the kernel command line.
  guest = import "${pkgs.path}/nixos" {
    system = pkgs.system;
    configuration = { pkgs, ... }: {
      imports = [ "${netbootPath}" ];
      netboot.squashfsCompression = "gzip -Xcompression-level 1";
      documentation.enable = lib.mkForce false;
      # Warnings from the journal go to the console, so a failed guest boot says why.
      boot.kernelParams = [ "console=ttyS0" "panic=-1" "systemd.journald.forward_to_console=1" "systemd.journald.max_level_console=warning" ];
      networking.usePredictableInterfaceNames = false;
      networking.useDHCP = false;
      networking.firewall.enable = false;
      # NetworkManager would flush the address the workload sets from the command line.
      networking.networkmanager.enable = lib.mkForce false;
      networking.wireless.enable = lib.mkForce false;
      systemd.services.workload = {
        description = "serve a durable counter kept on the replicated disk";
        wantedBy = [ "multi-user.target" ];
        path = with pkgs; [ coreutils gnugrep iproute2 util-linux e2fsprogs busybox ];
        script = builtins.readFile ./guest-workload.sh;
      };
    };
  };
  kernel = "${guest.config.system.build.kernel}/${guest.config.system.boot.loader.kernelFile}";
  initrd = "${guest.config.system.build.netbootRamdisk}/initrd";
  cmdline = "init=${guest.config.system.build.toplevel}/init ${toString guest.config.boot.kernelParams}";

  node = idx: {
    imports = [ self.nixosModules.expanse ../modules/storage-test.nix ];
    nixpkgs.overlays = [ (final: prev: { expanse = self.packages.${prev.system}.expanse; }) ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = lib.fixedWidthString 8 "0" (lib.toLower (lib.toHexString idx));
    expanse.hostname = "n${toString idx}";
    expanse.storage-test.enable = true;
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalInterface = "eth1";
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.qemu_kvm ];
    boot.kernelModules = [ "kvm" ];
    virtualisation.cores = 2;
    virtualisation.memorySize = 4608;
  };
in
{
  name = "expanse-cluster-vm-workloads-${toString nodeCount}";

  nodes = lib.genAttrs (map (i: "n${toString i}") (lib.range 1 nodeCount)) (n: { ... }: node (lib.toInt (lib.removePrefix "n" n)))
    # Outside the cluster, polling the services the way a client would.
    // {
      watcher = { pkgs, ... }: {
        environment.systemPackages = [ pkgs.curl ];
        virtualisation.cores = 2;
        virtualisation.memorySize = 1024;
      };
    };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    KERNEL = "${kernel}"
    INITRD = "${initrd}"
    CMDLINE = "${cmdline}"
    POLLER = "${./watcher-poll.sh}"
    NODE_COUNT = ${toString nodeCount}
    CORES_PER_VM = ${toString coresPerVM}
    ${builtins.readFile ./python/cluster_vm_workloads.py}
  '';
}
