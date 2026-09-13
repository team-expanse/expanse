# §6 cluster-linearizable: 100 write→immediate-read rounds with the
# writer rotating across nodes; reads on the other two nodes must never
# return stale data (G3.5).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-cluster-linearizable";

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

    form("linearizable")

    nodes = [n1, n2, n3]

    with subtest("100 put→read rounds, writer rotating, zero stale reads (G3.5)"):
        stale = []
        for i in range(100):
            w = nodes[i % 3]
            key = f"/test/k{i}"
            val = f"value{i}"
            rc, out = kv(w, f"put {key} {val}")
            if rc != 0:
                stale.append(f"write {key} failed on {w.name}: {out}")
                continue
            # Immediately read on the other two nodes: linearizable gets
            # must see the just-acked write (forwarded to the leader).
            for r in nodes:
                if r is w:
                    continue
                rc2, out2 = kv(r, f"get {key}")
                if rc2 != 0 or out2.strip() != val:
                    stale.append(f"{r.name} read {key}={out2.strip()!r}, want {val!r} (wrote on {w.name})")
        assert not stale, f"{len(stale)} stale/failed reads, first 3: {stale[:3]}"
        print(f"linearizable: 100 rounds, {len(stale)} violations")

    with subtest("spot-check final state from every node"):
        for m in nodes:
            rc, out = kv(m, "get /test/k99")
            assert out.strip() == "value99", f"{m.name} bad final read: {out}"
  '';
}
