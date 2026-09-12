{ config, pkgs, lib, ... }:
{
  options = {
    expanse.enable = lib.mkEnableOption "enable expanse package";
  };

  config = lib.mkIf config.expanse.enable {
    environment.systemPackages = [ pkgs.expanse ];
  };
}