# §6 cluster-generations: applying desired state creates generations;
# diff shows the changes; rollback creates a NEW generation whose hash
# matches the target's; every node converges to the rolled-back state
# within 60 s; history is preserved (G3.11).
#
# Convergence shape: a base file resource is applied on all three nodes
# first (generation 2 = the rollback target), then the A/B/C churn
# happens on one node's tree. After rollback every node's managed state
# must match the target generation's desired state.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-cluster-generations";

  nodes = {
    n1 = { ... }: {
      imports = [ self.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000001";
      expanse.hostname = "n1";
      virtualisation.memorySize = 1536;
    };
    n2 = { ... }: {
      imports = [ self.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000002";
      expanse.hostname = "n2";
      virtualisation.memorySize = 1536;
    };
    n3 = { ... }: {
      imports = [ self.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000003";
      expanse.hostname = "n3";
      virtualisation.memorySize = 1536;
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}

    def apply_file(m, rid, path, content):
        rc, out = m.execute(
            "expanse ctl resource apply --socket /run/expanse/agent.sock -f - <<'EOF'\n"
            f"{rid}:\n  type: file\n  path: {path}\n  content: {content}\n  mode: \"0644\"\nEOF\n"
        )
        assert rc == 0, f"{m.name} apply failed: {out}"

    def gen_hash(m, n):
        rc, out = m.execute(f"expanse ctl generation show {n} --socket /run/expanse/agent.sock 2>&1")
        assert rc == 0, out
        for ln in out.splitlines():
            if ln.startswith("hash:"):
                return ln.split("hash:")[1].strip()
        raise AssertionError(f"no hash for generation {n}: {out}")

    form("generations")

    with subtest("base state on all nodes → generations 2-4 (one per apply)"):
        for m in [n1, n2, n3]:
            apply_file(m, "file:/etc/gen-base", "/etc/gen-base", "base")
        rc, out = n1.execute("expanse ctl generation list --socket /run/expanse/agent.sock 2>&1")
        nums = [int(ln.split()[0]) for ln in out.splitlines()
                if ln.split() and ln.split()[0].isdigit()]
        assert nums and nums[-1] == 4, f"expected generation 4 as current: {out}"
        for m in [n1, n2, n3]:
            rc, out = kv(m, "list /node/")
            print(f"{m.name} /node/ keys: {out!r}")
            rc, out = m.execute("expanse ctl resource list --socket /run/expanse/agent.sock 2>&1")
            print(f"{m.name} resources: {out!r}")

    with subtest("apply A, B, C on n1's tree → generations 5, 6, 7"):
        apply_file(n1, "file:/etc/gen-base", "/etc/gen-base", "alpha")   # gen 5
        apply_file(n1, "file:/etc/gen-base", "/etc/gen-base", "bravo")   # gen 6
        apply_file(n1, "file:/etc/gen-base", "/etc/gen-base", "charlie") # gen 7
        rc, out = n1.execute("expanse ctl generation list --socket /run/expanse/agent.sock 2>&1")
        nums = [int(ln.split()[0]) for ln in out.splitlines()
                if ln.split() and ln.split()[0].isdigit()]
        assert [1, 2, 3, 4, 5, 6, 7] == nums[-7:], f"generations wrong: {out}"

    with subtest("diff 2 6 shows the expected change"):
        rc, out = n1.execute("expanse ctl generation diff 2 6 --socket /run/expanse/agent.sock 2>&1")
        assert rc == 0, out
        assert "gen-base" in out, f"diff does not mention gen-base: {out}"

    with subtest("rollback 2 → new generation with generation 2's hash"):
        h2 = gen_hash(n1, 2)
        rc, out = n1.execute("expanse ctl generation rollback 2 --socket /run/expanse/agent.sock 2>&1")
        assert rc == 0, out
        m = re.search(r"(\d+)", out)
        assert m, f"rollback did not report the new generation: {out}"
        new_gen = int(m.group(1))
        assert new_gen == 8, f"expected new generation 8, got: {out}"
        assert gen_hash(n1, new_gen) == h2, \
            f"rollback hash mismatch: gen{new_gen}={gen_hash(n1, new_gen)} gen2={h2}"

    with subtest("all nodes converge to the rolled-back state within 60 s"):
        deadline = time.time() + 60
        states = ["", "", ""]
        while time.time() < deadline:
            states = []
            for m in [n1, n2, n3]:
                rc, out = m.execute("cat /etc/gen-base 2>/dev/null || echo MISSING")
                states.append(out.strip())
            if states == ["base"] * 3:
                break
            time.sleep(2)
        assert states == ["base"] * 3, f"not converged to state A: {states}"

    with subtest("history preserved: generations 3, 4, 5 still listed"):
        rc, out = n1.execute("expanse ctl generation list --socket /run/expanse/agent.sock 2>&1")
        nums = [int(ln.split()[0]) for ln in out.splitlines()
                if ln.split() and ln.split()[0].isdigit()]
        for want in (5, 6, 7):
            assert want in nums, f"generation {want} missing from history: {out}"
  '';
}
