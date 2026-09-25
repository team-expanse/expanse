# Phase 11, Stream A (X1): 3 systemd-nspawn containers on THIS host, each capped
# at the advertised minimum (2 vCPU / 4 GB RAM) with a real dedicated disk
# (sdb/sdc/sdd) backing a real DRBD-replicated volume -- standing in for 3
# separate real machines without installing onto (or wiping) this host's own
# disk. See ./README.md for the full design, what this does and does not
# prove versus an actual bare-metal install, and how to tear it down.
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

  disks = {
    n1 = "/dev/sdb";
    n2 = "/dev/sdc";
    n3 = "/dev/sdd";
  };
  addrs = {
    n1 = "192.168.1.1";
    n2 = "192.168.1.2";
    n3 = "192.168.1.3";
  };

  mkContainer = idx: let
    name = "n${toString idx}";
    disk = disks.${name};
    addr = addrs.${name};
  in {
    privateNetwork = true;
    hostBridge = "br-expanse";
    # A prefix length is required when hostBridge is set (systemd-nspawn(5)).
    localAddress = "${addr}/24";
    bindMounts.${disk} = {
      hostPath = disk;
      isReadOnly = false;
    };
    # device-mapper's control device is not among nspawn's small set of
    # auto-populated private /dev nodes, and LVM tries to mknod it itself the
    # first time -- found live ("/dev/mapper/control: mknod failed: Operation
    # not permitted") when expanse-scratch-vg.service ran. CAP_MKNOD lets that
    # mknod succeed; allowedDevices grants the cgroup permission to actually
    # use it once created. The device itself is host-kernel-wide (dm operates
    # per-VG-name, not per-namespace), same as the real disk it manages.
    allowedDevices = [
      { node = disk; modifier = "rwm"; }
      { node = "/dev/mapper/control"; modifier = "rwm"; }
    ];
    additionalCapabilities = [ "CAP_MKNOD" ];
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
      expanse.agent.storageVG = "expanse";
      expanse.agent.storagePool = "pool";

      # nixpkgs' own virtualisation/container-config.nix (auto-applied under
      # boot.isContainer) defaults services.lvm.enable off, on the assumption a
      # container has no block devices of its own to manage -- ours does. Found
      # live via `nix eval`: a plain conflicting definition, not a mkDefault, so
      # this needs mkForce to win.
      services.lvm.enable = lib.mkForce true;

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
      fileSystems."/" = {
        # "/" specifically (unlike /nix, /persist) hard-requires a device string --
        # found live via `nix eval`: "No device specified for mount point '/'." This
        # is never actually consumed (nspawn owns the container's real root from
        # outside), so any inert placeholder is fine.
        device = lib.mkForce "rootfs";
        fsType = lib.mkForce "none";
        neededForBoot = lib.mkForce false;
      };
      fileSystems."/nix" = { device = lib.mkForce "rootfs"; fsType = lib.mkForce "none"; neededForBoot = lib.mkForce false; };
      fileSystems."/persist" = { device = lib.mkForce "rootfs"; fsType = lib.mkForce "none"; neededForBoot = lib.mkForce false; };

      # impermanence.nix's own root-wipe mechanism is an initrd-stage unit
      # (expanse-impermanence-rollback / expanse-persist-init) -- a container
      # never runs an initrd, so that unit never fires. Its 4 regular (non-
      # initrd) bind-mount fileSystems entries for /etc/machine-id, /var/lib/
      # nixos, /var/lib/systemd and /root/.ssh are still declared and DO run
      # at normal boot, though -- they just need their /persist/... sources
      # to already exist, which setup.sh pre-creates once, on the host side
      # of the /persist bind mount, before the container's first start.
      # Impermanence itself (root wiped every reboot) is simply not being
      # tested here -- X1 is about steady-state agent CPU/RSS and DRBD
      # failover timing, not the reboot-wipe mechanism.

      # The dedicated disk backing the real DRBD-replicated volume: create
      # the "expanse" VG + thin pool once, exactly like nix/modules/agent.nix
      # expects a real install's disko layout to have already done, and like
      # nix/tests/modules/storage-test.nix does for the VM tests -- reusing
      # that same idempotent, never-wipes-an-existing-VG shape.
      systemd.services.expanse-scratch-vg = {
        description = "Create the expanse LVM volume group on the dedicated disk (X1 container harness)";
        wantedBy = [ "multi-user.target" ];
        before = [ "expansed.service" ];
        after = [ "systemd-udev-settle.service" ];
        path = [ pkgs.lvm2 pkgs.thin-provisioning-tools ];
        unitConfig.DefaultDependencies = "no";
        serviceConfig.Type = "oneshot";
        serviceConfig.RemainAfterExit = true;
        script = ''
          vgchange -ay expanse >/dev/null 2>&1 || true
          # Check the POOL, not just the VG: a real prior run on this exact host got
          # partway (VG created) before the thin-pool step itself failed (missing
          # CAP_MKNOD, fixed separately) -- checking only `vgs expanse` would have
          # treated that half-built state as "already done" and never retried the
          # pool, found live rather than assumed.
          if lvs expanse/pool >/dev/null 2>&1; then
            echo "expanse-scratch-vg: volume group + thin pool already exist"
            exit 0
          fi
          vgs expanse >/dev/null 2>&1 || vgcreate expanse ${disk}
          lvcreate --yes --type thin-pool -l 80%FREE -n pool expanse
        '';
      };
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
  systemd.services."container@n1".serviceConfig = { CPUQuota = "200%"; MemoryMax = "4G"; };
  systemd.services."container@n2".serviceConfig = { CPUQuota = "200%"; MemoryMax = "4G"; };
  systemd.services."container@n3".serviceConfig = { CPUQuota = "200%"; MemoryMax = "4G"; };
}
