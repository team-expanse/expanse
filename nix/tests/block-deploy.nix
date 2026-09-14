# §8 block-deploy: deploy util/echo replicas=3 on a 3-node cluster;
# assert placed on 3 distinct nodes, all Running ≤ 30 s, all respond
# (G4.5). Also exercises the full T20.5 pipeline: API → leader
# controller → bridge → agent reconciler → systemd units.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-block-deploy";

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
      environment.systemPackages = with pkgs; [ openssl curl jq ];
      virtualisation.memorySize = 2048;
                # Tight reconcile period: the 30 s deploy budget (G4.5) must cover
            # placement + bridge + follower reconcile + promotion.
            expanse.agent.period = "5s";
            expanse.agent.blocksCatalog = ../blocks;
            expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
            environment.etc."expanse/blocks-flake".source = ../blocks-flake;
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
      environment.systemPackages = with pkgs; [ openssl curl ];
      virtualisation.memorySize = 2048;
                # Tight reconcile period: the 30 s deploy budget (G4.5) must cover
            # placement + bridge + follower reconcile + promotion.
            expanse.agent.period = "5s";
            expanse.agent.blocksCatalog = ../blocks;
            expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
            environment.etc."expanse/blocks-flake".source = ../blocks-flake;
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
      environment.systemPackages = with pkgs; [ openssl curl ];
      virtualisation.memorySize = 2048;
                # Tight reconcile period: the 30 s deploy budget (G4.5) must cover
            # placement + bridge + follower reconcile + promotion.
            expanse.agent.period = "5s";
            expanse.agent.blocksCatalog = ../blocks;
            expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
            environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}

    form("test")

    with subtest("deploy util/echo replicas=3"):
        deploy(n1, "web", echo_yaml("web", 3, 18080, "deploy-test\n"))

    with subtest("all replicas Running within 30 s (G4.5)"):
        b = wait_phase(n1, "web", ["RUNNING"], 30)
        assert len(placement_nodes(b)) == 3, \
            f"placed on {placement_nodes(b)}, want 3 distinct nodes: {b.get('status')}"

    with subtest("each replica is an active systemd unit on its node"):
        for idx in [0, 1, 2]:
            node = replica_node(b, idx)
            assert node is not None, f"replica {idx} unplaced: {b}"
            m = {"n1": n1, "n2": n2, "n3": n3}[node]
            assert unit_running(m, "default", "web", idx), \
                f"replica {idx} unit inactive on {node}"

    with subtest("every replica responds on :18080"):
        for node in placement_nodes(b):
            m = {"n1": n1, "n2": n2, "n3": n3}[node]
            echo_responds(m, 18080, "deploy-test\n", node="localhost")
  '';
}
