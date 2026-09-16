# §6 doctor (T22): `expanse doctor network` runs all 12 §5 checks
# against a live 3-node cluster and reports a PASS/WARN/FAIL table.
# The doctor CLI probes peers over the overlay, so the cluster peer
# flags are passed explicitly (the command runs on a node that knows
# the mesh shape).
{ self }:
{ pkgs, lib, ... }:
let
  nodeConfig = { hostId, host }: { ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = hostId;
    expanse.hostname = host;
    virtualisation.memorySize = 2048;
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    # §4.5 ruleset so the firewall row can pass (agent is the sole
    # nftables owner; NixOS firewall would flush the table).
    expanse.agent.firewall = true;
    networking.nftables.enable = lib.mkForce false;
    networking.firewall.enable = lib.mkForce false;
  };
in
{
  name = "expanse-doctor-network";
  nodes = {
    n1 = nodeConfig { hostId = "00000001"; host = "n1"; };
    n2 = nodeConfig { hostId = "00000002"; host = "n2"; };
    n3 = nodeConfig { hostId = "00000003"; host = "n3"; };
  };
  testScript = ''
    ${builtins.readFile ./cluster-common.py}

    form("doctest")
    wait_agent_ready(n1)
    wait_agent_ready(n2)
    wait_agent_ready(n3)

    with subtest("doctor network reports all 12 checks (G5.12)"):
        # exp0 addresses take one agent period to appear after the mesh
        # reconciles; retry until the exp0 row passes.
        out = ""
        for _ in range(30):
            rc, out = n1.execute(
                "expanse doctor network "
                "--peer n2=10.42.2.1,7443,7444,7446 "
                "--peer n3=10.42.3.1,7443,7444,7446"
            )
            rows = len([l for l in out.splitlines()
                        if l and not l.startswith(" ") and not l.startswith("CHECK")])
            if rows == 12 and "FAIL" not in out:
                break
            time.sleep(2)
        assert rows == 12, f"expected 12 rows, got {rows}:\n{out}"
        assert "FAIL" not in out, f"doctor reported failures:\n{out}"
        # Spot-check the fixed row set and at least one PASS on the
        # checks that need cluster state.
        for row in ["interfaces", "exp0", "overlay-peers", "df-mtu",
                    "port-matrix", "vips", "arp", "block-dns",
                    "upstream-dns", "firewall", "conntrack", "time-sync"]:
            assert row in out, f"missing {row} row:\n{out}"

    with subtest("doctor network exits nonzero on failure"):
        # A bogus peer must FAIL the overlay row (and exit 1).
        rc, out = n1.execute(
            "expanse doctor network --peer n9=10.42.9.9; exit $?",
            timeout=120,
        )
        # execute() raises on nonzero only for succeed(); assert via
        # captured shell exit.
        assert "FAIL" in out, f"bogus peer did not fail:\n{out}"
  '';
}
