# Phase 11, Stream A (X1): 3 systemd-nspawn containers on THIS host, each capped
# at the advertised minimum (2 vCPU / 4 GB RAM), measuring expansed's idle
# CPU/RSS without a hypervisor. No disks, LVM or DRBD: containers share one
# kernel, so those collide across nodes -- see ./README.md.
#
# Imported by /etc/nixos/configuration.nix (one line, added by setup.sh,
# clearly marked so it is easy to find and remove). Reuses
# self.nixosModules.expanse completely unchanged -- the SAME module every
# real install and every nixosTest VM node already uses -- plus two
# container-specific overrides, both narrowly scoped to the one real
# difference that matters here: a container has no boot/initrd stage of its
# own, unlike a VM or a real machine.
{ config, pkgs, lib, ... }:
let
  exp = builtins.getFlake "git+file:///home/jaredm/expanse";
  system = pkgs.system;

  addrs = {
    n1 = "192.168.1.1";
    n2 = "192.168.1.2";
    n3 = "192.168.1.3";
  };

  mkContainer = idx: let
    name = "n${toString idx}";
    addr = addrs.${name};
  in {
    privateNetwork = true;
    hostBridge = "br-expanse";
    # A prefix length is required when hostBridge is set (systemd-nspawn(5)).
    localAddress = "${addr}/24";
    # Started explicitly by setup.sh, not on every host boot.
    autoStart = false;

    config = { config, pkgs, lib, ... }: {
      imports = [ exp.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = exp.packages.${system}.expanse; })
      ];

      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "0000000${toString idx}";
      expanse.hostname = name;
      expanse.agent.raftAdvertise = "${addr}:7444";
      # storageVG stays at its "" default: the agent's volume stack (LVM/DRBD) is off.

      # dbus-broker inside this unprivileged nspawn container hits a real, repeating
      # failure -- "ERROR launcher_run_child: No medium found" / "ERROR service_add:
      # Transport endpoint is not connected", "Exiting due to fatal error: -107" --
      # found live. dbus.socket's own socket activation re-triggers dbus-broker.service
      # on every fresh connection attempt, forever, since the underlying error never
      # clears -- this keeps the container's own systemd permanently mid-boot (new jobs
      # perpetually queued), which is why it never signals readiness to nspawn
      # regardless of how high TimeoutStartSec is set (found live: still timed out even
      # at 180s). A known class of dbus-broker/unprivileged-nspawn incompatibility, not
      # something specific to this config.
      #
      # First tried disabling D-Bus entirely (services.dbus.enable = false) on the
      # assumption expansed has no D-Bus dependency -- true, but systemd-logind DOES
      # actually need it at runtime despite only Wants=-ing it: found live,
      # "Failed to connect to system bus: No such file or directory" / "Failed to fully
      # start up daemon", looping exactly like dbus-broker had. Wants= only means
      # systemd will still try to start logind even if dbus never starts; it does not
      # mean logind tolerates dbus's absence once running.
      #
      # Switching to the classic dbus-daemon implementation instead of disabling D-Bus
      # outright: the ENOMEDIUM failure is in dbus-broker's own launcher_run_child code
      # path specifically, which classic dbus-daemon doesn't share, so this keeps D-Bus
      # (and logind) actually functional while sidestepping dbus-broker's bug.
      services.dbus.implementation = "dbus";

      # storage.nix (imported by self.nixosModules.expanse, unconditionally
      # under expanse.node.enable) marks "/", "/nix" and "/persist"
      # neededForBoot for a real install, where disko has declared them as
      # real fileSystems entries with a device. A container gets all three
      # from nspawn's own external bind mounts instead (never declared in
      # this module's own fileSystems), so there is no device to wait on;
      # left at true this SEEMS to evaluate fine today (device defaults to
      # null, no hard assertion trips) but the mount-unit generator's actual
      # runtime handling of a neededForBoot entry with no device is not
      # something this design was able to verify without a real boot, so it
      # is force-disabled here rather than trusted.
      # fsType has no default in NixOS's own filesystems.nix; boot.supportedFilesystems'
      # own zfs-detection iterates every fileSystems entry's fsType, forcing its
      # evaluation even under boot.isContainer -- so each of these three needs a real,
      # if inert, declaration. Found live via `nix eval` against this exact module, not
      # assumed (a stock, minimal container example never hits this because it doesn't
      # import this much of the real module tree).
      # `noauto` on all three: found live that omitting it actually breaks the boot,
      # not just leaves harmless dead config as assumed. neededForBoot = false only
      # keeps a fileSystems entry out of the *early* boot path; without noauto it is
      # still pulled into local-fs.target as an ordinary REQUIRED mount, and since
      # "rootfs" isn't a real, mountable device, that mount fails ("nix.mount ...
      # failed") and -- being required -- escalates the whole boot into
      # `systemctl is-system-running` = maintenance (confirmed via `systemctl
      # --failed` inside a real running container). noauto means systemd never
      # attempts to mount these at all, which is correct: nspawn already provides
      # the real "/", "/nix" and "/persist" from outside before the container's own
      # systemd even starts: these entries exist only to satisfy the NixOS module
      # system's own eval-time fsType/device requirements (see above), nothing more.
      fileSystems."/" = {
        # "/" specifically (unlike /nix, /persist) hard-requires a device string --
        # found live via `nix eval`: "No device specified for mount point '/'." This
        # is never actually consumed (nspawn owns the container's real root from
        # outside), so any inert placeholder is fine.
        device = lib.mkForce "rootfs";
        fsType = lib.mkForce "none";
        neededForBoot = lib.mkForce false;
        options = [ "noauto" ];
      };
      fileSystems."/nix" = { device = lib.mkForce "rootfs"; fsType = lib.mkForce "none"; neededForBoot = lib.mkForce false; options = [ "noauto" ]; };
      fileSystems."/persist" = { device = lib.mkForce "rootfs"; fsType = lib.mkForce "none"; neededForBoot = lib.mkForce false; options = [ "noauto" ]; };

      # impermanence.nix's own root-wipe mechanism is an initrd-stage unit
      # (expanse-impermanence-rollback / expanse-persist-init) -- a container
      # never runs an initrd, so that unit never fires. Its 4 regular (non-
      # initrd) bind-mount fileSystems entries for /etc/machine-id, /var/lib/
      # nixos, /var/lib/systemd and /root/.ssh are still declared and DO run
      # at normal boot, though -- they just need their /persist/... sources
      # to already exist, which setup.sh pre-creates once, on the host side
      # of the /persist bind mount, before the container's first start.
      # Impermanence itself (root wiped every reboot) is simply not being
      # tested here -- X1 is about steady-state agent CPU/RSS, not the
      # reboot-wipe mechanism.
    };
  };
in
{
  boot.enableContainers = true;

  # Isolated bridge, no physical uplink: containers reach each other over it
  # directly (matching the nixosTest VM tests' own single-vlan eth1 model),
  # never touching the host's real LAN. Plain `ip link`, not
  # networking.bridges.<name>: that option is part of NixOS's classic scripted
  # networking backend, a no-op on a NetworkManager-managed host (this one) --
  # found live ("Failed to add interface vb-n1 to bridge br-expanse: No such
  # device"), not assumed. A kernel bridge created this way exists and is
  # usable by nspawn's --network-bridge= regardless of which stack manages
  # every other interface on the host.
  systemd.services.expanse-perf-bridge = {
    description = "Create the isolated bridge for the X1 perf containers";
    wantedBy = [ "multi-user.target" ];
    before = [ "container@n1.service" "container@n2.service" "container@n3.service" ];
    path = [ pkgs.iproute2 ];
    serviceConfig.Type = "oneshot";
    serviceConfig.RemainAfterExit = true;
    script = ''
      ip link show br-expanse >/dev/null 2>&1 || ip link add br-expanse type bridge
      ip link set br-expanse up
    '';
  };

  containers.n1 = mkContainer 1;
  containers.n2 = mkContainer 2;
  containers.n3 = mkContainer 3;

  # The whole-node budget this stream is measuring (ARCHITECTURE.md §8):
  # 2 vCPU, 4 GB RAM per node, enforced as a hard systemd resource cap on
  # each container's own service, not just declared informationally.
  #
  # TimeoutStartSec: systemd's unmodified 1min default -- found live, "Job for
  # container@n1.service failed because a timeout was exceeded" -- is tighter than
  # setup.sh's own readiness poll already tolerates (up to 180s *after* the unit
  # reports started). Several known-benign unit failures inside the container
  # (chronyd: no CAP_SYS_TIME; dbus-broker; systemd-machine-id-commit) restart-loop
  # during boot and burn real wall-clock time, and 3 containers starting at once under the CPUQuota cap adds contention on top
  # of that -- 60s is simply too tight for what this harness already accepts as a healthy boot.
  # Matching setup.sh's own 180s budget here instead of leaving this as an
  # intermittent flake.
  # nixos-containers.nix itself already sets TimeoutStartSec = "1min" as a plain
  # definition, not a mkDefault -- found live via `nix eval`, needs mkForce to win.
  systemd.services."container@n1".serviceConfig = { CPUQuota = "200%"; MemoryMax = "4G"; TimeoutStartSec = lib.mkForce "180s"; };
  systemd.services."container@n2".serviceConfig = { CPUQuota = "200%"; MemoryMax = "4G"; TimeoutStartSec = lib.mkForce "180s"; };
  systemd.services."container@n3".serviceConfig = { CPUQuota = "200%"; MemoryMax = "4G"; TimeoutStartSec = lib.mkForce "180s"; };
}
