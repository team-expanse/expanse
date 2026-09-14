# Minimal closure flake shipped to block nodes (T21): one store path per
# block type so the runtime's nix build step succeeds without network or
# nixpkgs (the attribute is a plain path — nix just copies it to the
# store). Real per-type closures arrive with the shipped-block workloads
# (T24); the pipeline (build → cache → unit) is what these prove.
{
  outputs = _: {
    packages.x86_64-linux.util-echo = ./.;
  };
}
