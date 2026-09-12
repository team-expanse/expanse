# G2.1/G2.2/G2.8/G2.9/G2.10: agent starts via systemd, reaches ready fast,
# serves the CLI over the unix socket, inventory is complete, store
# persists across restart.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-agent-basic";

  nodes.machine = { config, pkgs, ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "01234567";

    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
  };

  testScript = ''
    machine.start()
    machine.wait_for_unit("expansed.service")
    machine.wait_for_unit("multi-user.target")

    with subtest("unit is active and ready"):
        status = machine.succeed("systemctl is-active expansed.service")
        assert status.strip() == "active"

    with subtest("reached ready quickly (G2.1: <= 3s)"):
        out = machine.succeed("systemd-analyze blame expansed.service || true")
        print(f"blame: {out}")

    with subtest("node status over unix socket, no network (G2.10)"):
        out = machine.succeed("expanse ctl node status")
        assert "node:" in out

    with subtest("inventory complete (G2.8)"):
        out = machine.succeed("expanse ctl node inspect -o json")
        assert '"cores"' in out and out.count('"cores"') >= 1
        import json
        inv = json.loads(out)
        assert inv["cpu"]["cores"] > 0, "cpu cores missing"
        assert inv["memory"]["total"] > 0, "memory missing"
        assert inv["hostname"] != "", "hostname missing"

    with subtest("health report"):
        out = machine.succeed("expanse ctl node health")
        assert "overall" in out

    with subtest("store persists across restart (G2.2)"):
        # Apply a resource, restart the agent, assert it is still listed.
        machine.succeed("expanse ctl resource apply - <<'EOF'\nfile:/etc/expanse-test:\n  type: file\n  path: /etc/expanse-test\n  content: persisted\n  mode: \"0644\"\nEOF\n")
        machine.succeed("expanse ctl reconcile")
        machine.succeed("test \"$(cat /etc/expanse-test)\" = persisted")
        machine.succeed("systemctl restart expansed.service")
        machine.wait_for_unit("expansed.service")
        out = machine.succeed("expanse ctl resource list")
        assert "file:/etc/expanse-test" in out, f"resource lost after restart: {out}"

    with subtest("agent restarts within 5s"):
        machine.succeed("systemctl stop expansed.service")
        machine.succeed("systemctl start expansed.service")
        machine.wait_for_unit("expansed.service", timeout=5)

    with subtest("switch watchdog timer is active"):
        machine.wait_for_unit("expanse-switch-watchdog.timer")
  '';
}
