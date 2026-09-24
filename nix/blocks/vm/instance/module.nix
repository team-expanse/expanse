# Block runtime module for vm/instance (PHASE-06-TASKS.md Stream A).
#
# One VM instance per replica, active/passive over the block's SINGLETON
# strategy (D3) — colocated with its raw volume's DRBD primary by the
# same P12 colocation filter iscsi/target already relies on, since the
# raw device backing the guest's disk is only openable on that node.
# No libvirt (D1, `ARCHITECTURE.md` A33 revised): qemu-kvm is exec'd
# directly, the same "exec upstream tooling" shape targetcli/smbd
# already use here — this module documents the unit's contract; the
# macvtap setup, disk-device resolution and qemu-kvm invocation itself
# are driven from Go (cmd/expanse-block-run's runVM), the same split
# db/postgres's own module.nix already has with bootstrapPostgres.
#
# Contract: import and call with:
#   {
#     pkgs      # nixpkgs
#     name      # instance name (systemd unit name)
#   }
{ pkgs, name }:
let
  unit = "expanse-block-${name}";
in
{
  systemd.services."${unit}" = {
    description = "expanse block ${name} (vm/instance)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    serviceConfig = {
      # A demotion/stop is a hard kill of the qemu-kvm child, by design
      # (X5: live migration/graceful guest quiesce is explicitly out of
      # scope this phase) — process death always closes qemu's own fd on
      # the raw device, so no separate release step is needed the way
      # LIO's kernel-held backstore needed one (contrast lioTarget's own
      # teardown()).
      Type = "simple";
      ExecStart = "${pkgs.expanse}/bin/expanse-block-run ${unit}";
      Restart = "on-failure";
      RestartSec = "5s";
      # CAP_NET_ADMIN (macvtap child creation, D2/A34) and /dev/kvm both
      # need root — the same class of privilege share/smb's setuid() and
      # iscsi/target's configfs tree already need (rootBlockTypes,
      # internal/blocks/wire/bridge.go).
      NoNewPrivileges = true;
    };
  };
}
