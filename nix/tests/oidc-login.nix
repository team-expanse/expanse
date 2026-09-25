# Phase 10 X3: OIDC login for the web UI, proven against a real OIDC
# provider (dexidp/dex, packaged in nixpkgs as `dex-oidc` -- "dex" itself
# is an unrelated desktop-entry utility), not a stub. A real browser's
# authorization-code round trip is driven hop by hop (following each
# real HTTP redirect dex itself issues, never a mocked token exchange),
# ending in a real store-backed auth.Session identical to password
# login's, including a check that it is honored on a *different* node
# (D2's "same session every other login path uses").
# The scenario is python/oidc_login_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  # A real bcrypt hash of dexPassword, computed at build time (not
  # hand-typed from memory) so the VM test's staticPasswords entry is
  # provably correct rather than guessed.
  dexPassword = "vm-test-oidc-password-do-not-log-me";
  dexPasswordHash = pkgs.runCommand "oidc-login-dex-password-hash"
    { nativeBuildInputs = [ (pkgs.python3.withPackages (ps: [ ps.bcrypt ])) ]; }
    ''
      python3 -c "
      import bcrypt
      print(bcrypt.hashpw(b'${dexPassword}', bcrypt.gensalt()).decode(), end=\"\")
      " > $out
    '';

  lint = pkgs.runCommand "oidc-login-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/oidc_login_main.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';

  nodeCommon = idx: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    environment.systemPackages = [ pkgs.curl ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-oidc-login";

  nodes = {
    n1 = { ... }: {
      imports = [ (nodeCommon 1) ];
      # The real IdP (D2's adoption pick's counterpart on the provider
      # side): run ad hoc from the test script, same reasoning as
      # observability-metrics.nix's Prometheus -- the client secret and
      # redirect URL don't exist until after `expanse ctl oidc
      # configure` runs, so this is not a bundled systemd service.
      environment.systemPackages = [ pkgs.curl pkgs.dex-oidc ];
    };
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    DEX_BIN = "${pkgs.dex-oidc}/bin/dex"
    DEX_PASSWORD = "${dexPassword}"
    DEX_PASSWORD_HASH = "${builtins.readFile dexPasswordHash}"
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/oidc_login_main.py}
  '';
}
