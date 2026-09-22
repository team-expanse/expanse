# Node identity and first-boot initialization.
{ config, pkgs, lib, ... }:
let
  expanseDir = "${config.expanse.persistDir}/expanse";
in
{
  config = lib.mkIf config.expanse.node.enable {
    # Generate identity once, before anything that needs it. The node-id
    # file is authoritative and `expanse node init` is idempotent.
    systemd.services.expanse-identity = {
      description = "Ensure Expanse node identity";
      after = [ "persist.mount" ];
      before = [ "expanse-firstboot.service" ];
      wantedBy = [ "multi-user.target" ];
      unitConfig.ConditionPathIsMountPoint = config.expanse.persistDir;
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
      };
      path = with pkgs; [ coreutils ];
      script = ''
        ${pkgs.expanse}/bin/expanse node init
        # Identity files are owned by the expanse system user (uid 990).
        chown -R expanse:expanse ${expanseDir}/identity
        chmod 0700 ${expanseDir}/identity
      '';
    };

    # First boot: identity, state dirs, network, marker. Idempotent —
    # if the marker exists, exit 0 immediately.
    systemd.services.expanse-firstboot = {
      description = "Expanse first-boot initialization";
      after = [ "persist.mount" "network-online.target" "expanse-identity.service" ];
      before = [ "expansed.service" ];
      wants = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];
      unitConfig.ConditionPathIsMountPoint = config.expanse.persistDir;
      path = with pkgs; [ expanse coreutils ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
      };
      script = ''
        set -eu
        marker=${expanseDir}/.firstboot-complete
        if [ -e "$marker" ]; then
          echo "expanse-firstboot: marker present, nothing to do"
          exit 0
        fi
        expanse node init
        install -d -m 0750 -o expanse -g expanse \
          ${expanseDir}/raft ${expanseDir}/secrets ${expanseDir}/blocks ${expanseDir}/volumes
        install -d -m 0700 -o expanse -g expanse ${expanseDir}/identity
        date -u +"%Y-%m-%dT%H:%M:%SZ" > "$marker"
        echo "expanse-firstboot: node initialized"
      '';
    };
  };
}
