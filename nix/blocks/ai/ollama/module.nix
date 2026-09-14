# Block runtime module for ai/ollama (PHASE04.md §3.3/§6).
#
# Deployed with requiredCapabilities: [gpu] and a spec.resources.devices
# GPU request; model blobs live on a large storage volume (mountPath).
#
# Contract: import and call with:
#   {
#     pkgs      # nixpkgs
#     name      # instance name (systemd unit name)
#     port      # API port, from spec.config.port
#     models    # list of model names to pull on start
#     keepAlive # model keep-alive duration
#     mountPath # model-storage volume mount point (spec.storage[0].mountPath)
#     user      # runtime user (default "expanse-block")
#   }
{ pkgs, name, port, models, keepAlive, mountPath, user ? "expanse-block" }:
let
  pullScript = pkgs.writeShellScript "expanse-block-${name}-pull" ''
    for m in ${pkgs.lib.concatMapStrings (m: "${m} ") models}; do
      ${pkgs.ollama}/bin/ollama pull "$m" || true
    done
  '';
in
{
  systemd.services."expanse-block-${name}" = {
    description = "expanse block ${name} (ai/ollama)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    serviceConfig = {
      ExecStart = "${pkgs.ollama}/bin/ollama serve";
      Environment = [
        "OLLAMA_HOST=0.0.0.0:${toString port}"
        "OLLAMA_MODELS=${mountPath}"
        "OLLAMA_KEEP_ALIVE=${keepAlive}"
      ];
      ExecStartPost = pullScript;
      User = user;
      Restart = "always";
      RestartSec = "1s";
      NoNewPrivileges = true;
      ProtectHome = true;
      PrivateTmp = true;
    };
  };
}
