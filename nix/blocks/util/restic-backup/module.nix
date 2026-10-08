# Block runtime module for util/restic-backup.
#
# One replica per node (DAEMONSET), running as root: each backs up the listed
# volumes whose DRBD primary is on its node, from a thin snapshot, with restic.
# expanse-block-run (restic.go) drives the loop.
#
# Contract: import and call with:
#   {
#     pkgs     # nixpkgs
#     name     # instance name (systemd unit name)
#     blockRun # the expanse-block-run binary
#     config   # JSON spec.config
#   }
{ pkgs, name, blockRun, config }:
{
  environment.systemPackages = [ pkgs.restic ];
  systemd.services."expanse-block-${name}" = {
    description = "expanse block ${name} (util/restic-backup)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    path = [ pkgs.restic pkgs.lvm2 pkgs.drbd pkgs.coreutils ];
    serviceConfig = {
      ExecStart = "${blockRun} ${name} --config ${config}";
      Restart = "always";
      RestartSec = "5s";
      NoNewPrivileges = true;
      ProtectHome = true;
      PrivateTmp = true;
    };
  };
}
