# G2.4/G2.5: reconcile converges a file resource in <= 5s, repairs drift,
# is idempotent (2nd run = 0 changes), and deletes removed resources.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-agent-reconcile";

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
    import json, time

    machine.start()
    machine.wait_for_unit("expansed.service")
    machine.wait_for_unit("multi-user.target")

    def changes():
        out = machine.succeed("expanse ctl node status -o json")
        # protojson emits 64-bit ints as strings.
        return int(json.loads(out)["changesApplied"])

    with subtest("apply a file resource"):
        machine.succeed("expanse ctl resource apply -f - <<'EOF'\nfile:/etc/expanse-test:\n  type: file\n  path: /etc/expanse-test\n  content: hello\n  mode: \"0644\"\nEOF\n")

    with subtest("converges within 5s (G2.5)"):
        deadline = time.time() + 5
        while time.time() < deadline:
            out = machine.succeed("cat /etc/expanse-test 2>/dev/null || true")
            if out.strip() == "hello":
                break
            time.sleep(0.2)
        assert out.strip() == "hello", "file not converged within 5s"
        mode = machine.succeed("stat -c %a /etc/expanse-test").strip()
        assert mode == "644", f"mode is {mode}"

    with subtest("watch-triggered drift repair"):
        machine.succeed("echo bad > /etc/expanse-test")
        deadline = time.time() + 10
        while time.time() < deadline:
            out = machine.succeed("cat /etc/expanse-test")
            if out.strip() == "hello":
                break
            time.sleep(0.2)
        assert out.strip() == "hello", "content drift not repaired"

    with subtest("mode drift repair"):
        machine.succeed("chmod 777 /etc/expanse-test")
        machine.succeed("expanse ctl reconcile")
        deadline = time.time() + 10
        while time.time() < deadline:
            mode = machine.succeed("stat -c %a /etc/expanse-test").strip()
            if mode == "644":
                break
            time.sleep(0.2)
        assert mode == "644", f"mode drift not repaired: {mode}"

    with subtest("idempotency: 2nd run applies 0 changes (G2.4)"):
        before = changes()
        machine.succeed("expanse ctl reconcile")
        machine.succeed("expanse ctl reconcile")
        after = changes()
        assert after == before, f"reconcile applied {after - before} changes on an in-sync system"

    with subtest("delete desired state -> file removed"):
        machine.succeed("expanse ctl resource delete file:/etc/expanse-test")
        deadline = time.time() + 10
        while time.time() < deadline:
            gone = machine.succeed("test -e /etc/expanse-test || echo gone").strip()
            if gone == "gone":
                break
            time.sleep(0.2)
        assert gone == "gone", "file not removed after resource delete"
        out = machine.succeed("expanse ctl resource list")
        assert "file:/etc/expanse-test" not in out, "resource still listed after delete"
  '';
}
