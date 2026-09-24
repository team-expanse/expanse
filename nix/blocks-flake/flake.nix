# Closure flake shipped to block nodes (T21/T24): one attribute per
# shipped block type, named "<category>-<name>" (slashes are not valid
# flake attrs); the agent builds <BlocksFlakeRef>#<attr> for a replica's
# type and caches the store path (internal/blocks/runtime/systemd/cache).
#
# The attributes are plain paths — nix copies them to the store with no
# network and no nixpkgs input, so in-VM builds are instant. The
# realized workloads live in expanse-block-run (binary-backed types exec
# upstream binaries from PATH; the deployment image ships them — the
# module.nix contract in nix/blocks/<cat>/<name>/ names the package).
# Real per-type closures replace these stubs when the module.nix-based
# deployment lands (Phase 06 storage / Phase 05 wiring).
{
  outputs = _: {
    packages.x86_64-linux.util-echo = ./.;
    packages.x86_64-linux.web-nginx = ./stubs/web-nginx;
    packages.x86_64-linux.web-whoami = ./stubs/web-whoami;
    packages.x86_64-linux.db-redis = ./stubs/db-redis;
    packages.x86_64-linux.monitor-node-exporter = ./stubs/monitor-node-exporter;
    packages.x86_64-linux.web-static-site = ./stubs/web-static-site;
    packages.x86_64-linux.ai-ollama = ./stubs/ai-ollama;
    packages.x86_64-linux.share-smb = ./stubs/share-smb;
    packages.x86_64-linux.iscsi-target = ./stubs/iscsi-target;
    packages.x86_64-linux.db-postgres = ./stubs/db-postgres;
  };
}
