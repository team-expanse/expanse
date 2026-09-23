# Block runtime module for iscsi/target (PHASE-04-TASKS.md Stream B).
#
# One LUN per instance (D5), active/passive over the block's SINGLETON
# strategy (D1, revised) — LIO itself has no clustering awareness; it is
# simply (re)configured wherever the block is scheduled, which the
# raw-storage colocation filter (Stream A, D2/D3) already guarantees is
# the volume's DRBD primary, the only node the raw device is actually
# openable on (nix/tests/iscsi-lio-drbd-secondary-probe.nix). Target
# identity (iqn, wwn) is pinned once (D4), never derived from anything
# node-specific, so every failover presents the initiator with the exact
# same device. Unlike smbd, LIO has no userspace daemon of its own: once
# its configfs tree is set up, the kernel serves I/O directly, so this
# unit is a oneshot that sets that state up and tears it down again on
# stop, not a long-running foreground process.
#
# Contract: import and call with:
#   {
#     pkgs          # nixpkgs
#     name          # instance name (systemd unit name)
#     iqn           # target IQN, pinned once at deploy (D4)
#     wwn           # backstore NAA WWN, pinned once at deploy (D4)
#     port          # LIO portal listen port, from spec.config.port
#     chapUser      # null or a CHAP username
#     chapPassword  # null or its secret
#     device        # bound raw volume's DRBD device path (D3), e.g. /dev/drbd7
#   }
{ pkgs, name, iqn, wwn, port, chapUser ? null, chapPassword ? null, device }:
let
  unit = "expanse-block-${name}";
  backstore = "expblk-${name}";
  tc = "${pkgs.targetcli-fb}/bin/targetcli";
  tpg = "/iscsi/${iqn}/tpg1";

  authAttrs = if chapUser != null then "authentication=1" else "authentication=0";

  # Idempotent: a restart on the same node finds its own prior objects
  # still live in the kernel's configfs tree — clear them before
  # recreating, the same "wipe and recreate before every start"
  # philosophy share/smb's ephemeral state reset uses for its own tdbs.
  setup = pkgs.writeShellScript "${unit}-setup" ''
    set -eu
    ${tc} /iscsi delete ${iqn} >/dev/null 2>&1 || true
    ${tc} /backstores/block delete ${backstore} >/dev/null 2>&1 || true
    ${tc} /backstores/block create name=${backstore} dev=${device} wwn=${wwn}
    ${tc} /iscsi create ${iqn}
    ${pkgs.lib.optionalString (port != 3260) ''
      ${tc} ${tpg}/portals delete 0.0.0.0 3260
      ${tc} ${tpg}/portals create 0.0.0.0 ${toString port}
    ''}
    ${tc} ${tpg}/luns create /backstores/block/${backstore}
    ${tc} ${tpg} set attribute ${authAttrs} generate_node_acls=1 demo_mode_write_protect=0
    ${pkgs.lib.optionalString (chapUser != null) ''
      ${tc} ${tpg} set auth userid=${chapUser} password=${chapPassword}
    ''}
  '';

  # A demotion needs this node's LIO to fully release the device before
  # DRBD can demote it out from under a still-open backstore.
  teardown = pkgs.writeShellScript "${unit}-teardown" ''
    ${tc} /iscsi delete ${iqn} >/dev/null 2>&1 || true
    ${tc} /backstores/block delete ${backstore} >/dev/null 2>&1 || true
  '';
in
{
  systemd.services."${unit}" = {
    description = "expanse block ${name} (iscsi/target)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    serviceConfig = {
      # LIO's configfs tree is root-only, the same class of privilege
      # share/smb's setuid() needs (PHASE-03-TASKS.md D1/D3) — neither is
      # a capability DynamicUser's random unprivileged uid can ever hold.
      Type = "oneshot";
      RemainAfterExit = true;
      ExecStart = "${setup}";
      ExecStop = "${teardown}";
      NoNewPrivileges = true;
    };
  };
}
