# The tty1 host console: `expanse console` (a read-only, ESXi-DCUI-like status screen)
# runs on the first VT in place of a login prompt. tty2+ (Alt+F2) and the serial console
# keep their gettys, so a shell is always one keystroke away.
{ config, pkgs, lib, ... }:
{
  config = lib.mkIf config.expanse.node.enable {
    # Masked, not just unwanted: logind's autovt@tty1 (an alias of getty@tty1) would
    # otherwise spawn a login on the console's VT the first time someone switches to it.
    systemd.services."getty@tty1".enable = lib.mkForce false;

    systemd.services.expanse-console = {
      description = "Expanse host console on tty1";
      after = [ "systemd-user-sessions.service" "getty-pre.target" "expanse-identity.service" ];
      conflicts = [ "getty@tty1.service" ];
      wantedBy = [ "multi-user.target" ];
      unitConfig.ConditionPathExists = "/dev/tty1";
      serviceConfig = {
        ExecStart = "${pkgs.expanse}/bin/expanse console";
        Restart = "always";
        RestartSec = "2s";
        StandardInput = "tty";
        StandardOutput = "tty";
        StandardError = "journal";
        TTYPath = "/dev/tty1";
        TTYReset = true;
        TTYVHangup = true;
        TTYVTDisallocate = true;
        # It runs forever on every node: keep it out of the way of real work.
        Nice = 10;
        MemoryMax = "64M";
      };
    };
  };
}
