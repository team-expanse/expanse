# Block runtime module for share/smb (PHASE-03-TASKS.md Stream B1).
#
# One SMB3 share per instance (D4), active/passive over the block's
# SINGLETON strategy (D1) — smbd itself has no clustering awareness; it is
# simply started wherever the block is scheduled, which §4.7's bind-mount
# and D2's colocation filter already guarantee is the volume's DRBD
# primary. All of smbd's own session state (D3: locking.tdb, brlock.tdb,
# connections.tdb and friends) lives under mountPath so it fails over with
# the data instead of resetting on every promotion.
#
# Contract: import and call with:
#   {
#     pkgs        # nixpkgs
#     name        # instance name (systemd unit name)
#     port        # smbd listen port, from spec.config.port
#     shareName   # share network name
#     path        # share content dir, relative to mountPath
#     readOnly    # deny writes
#     guestOk     # allow unauthenticated access
#     validUsers  # null or a space-separated user list
#     browseable  # list the share in network browsing
#     mountPath   # bound volume mount point (spec.storage[0].mountPath)
#   }
{ pkgs, name, port, shareName, path ? ".", readOnly ? false, guestOk ? true
, validUsers ? null, browseable ? true, mountPath }:
let
  stateDir = "${mountPath}/.smb-state";
  sharePath = "${mountPath}/${path}";
  unit = "expanse-block-${name}";

  conf = pkgs.writeText "expanse-block-${name}-smb.conf" ''
    [global]
      netbios name = ${builtins.substring 0 15 name}
      workgroup = WORKGROUP
      security = user
      server min protocol = SMB3
      smb ports = ${toString port}
      # Explicit, not auto-detected: a sandboxed unit with no AF_NETLINK
      # (the runtime-generated systemd unit's RestrictAddressFamilies)
      # can't enumerate interfaces at all and refuses to start without
      # this line ("Could not determine network interfaces").
      interfaces = 0.0.0.0/0
      bind interfaces only = no
      disable spoolss = yes
      load printers = no
      printing = bsd
      printcap name = /dev/null
      lock directory = ${stateDir}/lock
      state directory = ${stateDir}/state
      cache directory = ${stateDir}/cache
      private dir = ${stateDir}/private
      pid directory = ${stateDir}/run
      # A separate parameter from all the ones above — left unset it
      # defaults to /var/run/samba/ncalrpc, which does not exist here.
      ncalrpc dir = ${stateDir}/ncalrpc
      log file = ${stateDir}/log/log.%m
      log level = 1
      ${if guestOk then "map to guest = Bad User\nguest account = nobody" else ""}

    [${shareName}]
      path = ${sharePath}
      read only = ${if readOnly then "yes" else "no"}
      guest ok = ${if guestOk then "yes" else "no"}
      browseable = ${if browseable then "yes" else "no"}
      ${if validUsers != null then "valid users = ${validUsers}" else ""}
  '';
in
{
  systemd.services."${unit}" = {
    description = "expanse block ${name} (share/smb)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    serviceConfig = {
      # smbd needs to run as root: it setuid()s to the connecting user
      # per-session (or "nobody" for guest access), which requires the
      # capability, not just a directory permission.
      ExecStartPre = "+${pkgs.coreutils}/bin/install -d -m0750 ${sharePath} ${stateDir}/lock ${stateDir}/state ${stateDir}/cache ${stateDir}/private ${stateDir}/run ${stateDir}/log ${stateDir}/ncalrpc";
      # -l: smbd's own pre-config-parse startup logging uses this, not
      # the "log file" directive above. --debug-stdout: journald
      # convention every other block follows (samba otherwise logs only
      # to its own log file, never stderr, once past that early stage).
      ExecStart = "${pkgs.samba}/sbin/smbd --foreground --no-process-group --debug-stdout -l ${stateDir}/log -s ${conf}";
      Restart = "always";
      RestartSec = "1s";
      LimitNOFILE = 16384;
      NoNewPrivileges = true;
      PrivateTmp = true;
    };
  };
}
