# §6 cluster-form: bootstrap a 3-node cluster and assert full formation
# within 60 s — quorum 3/3, one leader, CA-signed certs on every node,
# generation 1 present everywhere (G3.1–G3.4, G3.11).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-cluster-form";

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
      environment.systemPackages = [ pkgs.openssl ];
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
      environment.systemPackages = [ pkgs.openssl ];
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
      environment.systemPackages = [ pkgs.openssl ];
      virtualisation.memorySize = 1536;
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}

    form("test")

    with subtest("all 3 nodes report a healthy 3-voter cluster, one leader (<= 60 s)"):
        rep = status(n1)
        assert "cluster:   test" in rep, f"no cluster report: {rep}"
        assert len(leaders(rep)) == 1, f"not exactly one leader: {rep}"
        assert has_node(rep, "n2") and has_node(rep, "n3"), f"n2/n3 missing: {rep}"
        # Every node must independently agree.
        for m in [n2, n3]:
            s = status(m)
            assert "quorum:    3/2" in s, f"{m.name} disagrees: {s}"

    with subtest("each node holds a cert signed by the cluster CA (G3.8)"):
        for m in [n1, n2, n3]:
            out = m.succeed(
                "openssl verify -CAfile /persist/expanse/ca/ca.pem "
                "/persist/expanse/tls/node-cert.pem"
            )
            assert ": OK" in out, f"{m.name} cert not CA-signed: {out}"

    with subtest("generation 1 exists on all nodes (G3.11)"):
        for m in [n1, n2, n3]:
            out = m.succeed("expanse ctl generation list --socket /run/expanse/agent.sock")
            assert "1" in out, f"{m.name} missing generation 1: {out}"
  '';
}
