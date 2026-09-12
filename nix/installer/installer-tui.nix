# Launch the interactive installer TUI on tty1 of the live ISO.
{ pkgs, lib, ... }:
{
  services.getty.autologinUser = lib.mkForce "root";
  systemd.services.expanse-installer-tui = {
    description = "Expanse installer TUI";
    after = [ "getty@tty1.service" ];
    wantedBy = [ "multi-user.target" ];
    unitConfig.ConditionPathExists = "/dev/tty1";
    serviceConfig = {
      StandardInput = "tty";
      StandardOutput = "tty";
      StandardError = "tty";
      TTYPath = "/dev/tty1";
      TTYReset = true;
      TTYVHangup = true;
      ExecStart = "${pkgs.expanse}/bin/expanse install --tui";
    };
  };
}
