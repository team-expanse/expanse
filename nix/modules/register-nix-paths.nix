# Nix 2.34.9 needs a home directory, but register-nix-paths (test VMs and the ISO) can run before
# /etc/passwd has root; $HOME is read first, so set it.
{ lib, options, ... }:
{
  config = lib.mkIf (options.virtualisation ? sharedDirectories || options ? isoImage) {
    systemd.services.register-nix-paths.environment.HOME = "/root";
  };
}
