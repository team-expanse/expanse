# Phase 13 A3: cpuModel host passes vmx through, so a guest can run KVM itself;
# unset keeps qemu's default model. The scenario is python/vm_instance_host_cpu.py.
{ self }:
{ pkgs, lib, ... }:
let
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "vm-instance-host-cpu-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/vm_instance_host_cpu.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';

  # vm-instance.nix's netboot guest; it records what CPU virtualization it sees on its raw disk.
  l2 = import "${pkgs.path}/nixos" {
    system = pkgs.system;
    configuration = {
      imports = [ "${pkgs.path}/nixos/modules/installer/netboot/netboot-minimal.nix" ];
      netboot.squashfsCompression = "gzip -Xcompression-level 1";
      documentation.enable = lib.mkForce false;
      boot.kernelParams = [ "console=ttyS0" "panic=-1" ];
      networking.useDHCP = false;
      systemd.services.cpu-report = {
        wantedBy = [ "multi-user.target" ];
        before = [ "multi-user.target" ];
        serviceConfig.Type = "oneshot";
        serviceConfig.RemainAfterExit = true;
        path = [ pkgs.coreutils pkgs.gnugrep pkgs.kmod pkgs.systemd ];
        script = builtins.readFile ./vm-instance-host-cpu-guest.sh;
      };
    };
  };
  kernel = "${l2.config.system.build.kernel}/${l2.config.system.boot.loader.kernelFile}";
  initrd = "${l2.config.system.build.netbootRamdisk}/initrd";
  cmdline = "init=${l2.config.system.build.toplevel}/init ${toString l2.config.boot.kernelParams}";
in
{
  name = "expanse-vm-instance-host-cpu";

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
    expanse.agent.raftAdvertise = "192.168.1.1:7444";
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalInterface = "eth1";
    boot.kernelModules = [ "kvm" ];
    # Two 2 GiB guests at once, plus the host's own headroom (vm-instance.nix sizes one at 4608).
    virtualisation.memorySize = 6656;
    virtualisation.cores = 4;
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
    ${builtins.readFile ./python/vm_instance_host_cpu.py}
  '';
}
