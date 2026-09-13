# §6 cluster-full-restart: stop all three nodes, start all three, and
# assert the state is intact and a leader is elected within 30 s
# (G3.4, G3.5).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-cluster-full-restart";

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

    form("full-restart")

    with subtest("seed state before the full stop"):
        for i in range(5):
            rc, out = kv(n1 if i % 2 == 0 else n2, f"put /restart/k{i} v{i}")
            assert rc == 0, out

    with subtest("stop ALL nodes"):
        for m in [n1, n2, n3]:
            m.succeed("systemctl stop expansed.service")

    with subtest("start all; leader within 30 s; state intact"):
        for m in [n1, n2, n3]:
            m.succeed("systemctl start expansed.service")
            m.wait_for_unit("expansed.service")

        rep = wait_quorum("3/2", 30)
        assert len(leaders(rep)) == 1, f"no single leader within 30 s: {rep}"

        for i in range(5):
            want = f"v{i}"
            for m in [n1, n2, n3]:
                deadline = time.time() + 15
                out = ""
                while time.time() < deadline:
                    rc, out = kv(m, f"get /restart/k{i}")
                    if rc == 0 and out.strip() == want:
                        break
                    time.sleep(1)
                assert out.strip() == want, f"{m.name} lost /restart/k{i}: {out!r}"
        print("full restart: state intact, leader re-elected")
  '';
}
