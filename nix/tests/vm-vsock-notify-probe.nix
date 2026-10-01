# Probe (workload health, step 1): does a systemd guest tell its host it finished booting over
# vsock (the vmm.notify_socket credential, as systemd-vmspawn uses)? Observations only; not a gate.
# Measured (systemd 260.2, QEMU 10.2.4): with vsock-stream:2:PORT the guest opens one connection per
# message, the peer CID names the guest, and READY=1 arrives with multi-user.target. READY=1 is also
# sent when a boot service fails and in emergency mode (then emergency.target, no multi-user.target).
# Shutdown sends X_SYSTEMD_UNIT_INACTIVE=multi-user.target, then X_SYSTEMD_SHUTDOWN and EXIT_STATUS.
# The bare vsock:2:PORT form sent nothing; a guest without init, or without the credential, sends
# nothing; a CID already in use fails QEMU's start ("Address already in use").
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "vm-vsock-notify-probe-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' ${./vm-vsock-notify-probe.py}
    touch $out
  '';

  netbootPath = "${pkgs.path}/nixos/modules/installer/netboot/netboot-minimal.nix";
  # A guest whose `probe.fail=1` command-line flag makes one boot-time service fail.
  guest = import "${pkgs.path}/nixos" {
    system = pkgs.system;
    configuration = { pkgs, ... }: {
      imports = [ "${netbootPath}" ];
      netboot.squashfsCompression = "gzip -Xcompression-level 1";
      documentation.enable = lib.mkForce false;
      boot.kernelParams = [ "console=ttyS0" "panic=-1" ];
      systemd.services.probe-mark = {
        wantedBy = [ "multi-user.target" ];
        serviceConfig.Type = "oneshot";
        script = ''
          echo "PROBE-MARK multi-user unit ran at $(cat /proc/uptime)" > /dev/ttyS0
          if grep -q probe.fail=1 /proc/cmdline; then exit 1; fi
        '';
      };
    };
  };
  kernel = "${guest.config.system.build.kernel}/${guest.config.system.boot.loader.kernelFile}";
  initrd = "${guest.config.system.build.netbootRamdisk}/initrd";
  cmdline = "init=${guest.config.system.build.toplevel}/init ${toString guest.config.boot.kernelParams}";
in
{
  name = "expanse-vm-vsock-notify-probe";
  nodes.n1 = { ... }: {
    virtualisation.memorySize = 8192;
    virtualisation.cores = 4;
    environment.systemPackages = [ pkgs.qemu_kvm pkgs.socat pkgs.kmod ];
  };
  testScript = ''
    # ${lint}
    KERNEL = "${kernel}"
    INITRD = "${initrd}"
    CMDLINE = "${cmdline}"
    SYSTEMD_VERSION = "${pkgs.systemd.version}"
    ${builtins.readFile ./vm-vsock-notify-probe.py}
  '';
}
