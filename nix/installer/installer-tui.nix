# Launch the interactive installer TUI from root's autologin shell on tty1 of the live ISO.
# A login shell has the PATH and EXPANSE_FLAKE the installer needs; quitting leaves that shell.
{ config, lib, ... }:
{
  options.expanse.installer.tuiArgs = lib.mkOption {
    type = lib.types.str;
    default = "";
    description = "Extra `expanse install --tui` flags (the install-tui VM test passes --skip-system-install).";
  };

  config = {
    services.getty.autologinUser = lib.mkForce "root";
    programs.bash.loginShellInit = ''
      if [ "$(tty)" = /dev/tty1 ] && [ ! -e /run/expanse-tui-started ]; then
        touch /run/expanse-tui-started
        expanse install --tui ${config.expanse.installer.tuiArgs}
        echo "Installer exited. Run 'expanse install --tui' to start it again."
      fi
    '';
  };
}
