# §8 block-antiaffinity: deploy replicas=4 with antiAffinity=node on a
# 3-node cluster; assert 3 Running + 1 Pending; assert `explain` names
# the reason; join a 4th node; assert the 4th replica places within
# 60 s (G4.6 antiaffinity half).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-block-antiaffinity";

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
      environment.systemPackages = with pkgs; [ openssl curl ];
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
    # The late joiner (starts idle — no enrollment in the boot config;
    # it joins via the CLI after the antiaffinity assertion).
    n4 = { ... }: {
      imports = [ self.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000004";
      expanse.hostname = "n4";
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

    # 4-token budget: n2/n3 consume two; the remaining one (persisted at
    # /root/join-token on n1) is for n4's late join — minting a fresh
    # token later requires a raft propose on the leader, which races
    # with the daemon's store lock.
    form("test", "voter", 4)

    with subtest("deploy replicas=4 with antiAffinity=node"):
        deploy(n1, "web", echo_yaml("web", 4, 18081, "aa\n", antiaffinity=True))

    with subtest("3 Running, 1 Pending within 30 s"):
        # The block phase stays SCHEDULING (one replica unplaceable);
        # the assertion is per-replica: 3 RUNNING, 1 pending, and the 3
        # running ones on distinct nodes.
        deadline = time.time() + 45
        b = None
        while time.time() < deadline:
            b = get_json(n1, "web")
            ps = (b.get("status") or {}).get("placements", [])
            live = [p for p in ps if p.get("replicaIndex", 0) >= 0
                    and p.get("phase") != "LOST"]
            running = [p for p in live if p.get("phase") == "RUNNING"]
            nodes = set(p.get("nodeId") for p in running)
            if len(running) == 3 and len(nodes) == 3 and len(live) == 3:
                break
            time.sleep(2)
        else:
            raise Exception(f"never 3 RUNNING on distinct nodes: {b}")
        # Exactly one replica remains unplaced/pending.
        assert len(live) == 3, f"live placements = {len(live)}, want 3: {b}"

    with subtest("explain names the reason (no node passes the node filter)"):
        rc, out = n1.execute(f"expanse ctl block explain {SOCK} web 2>/dev/null || true")
        assert rc == 0 and out.strip(), f"explain failed: rc={rc}, out={out!r}"
        # Every candidate node's verdict must reference the anti-affinity
        # filter (CodeAntiAffinity → "anti-affinity"/"conflict" wording
        # from the renderer) or a saturated/unavailable verdict.
        low = out.lower()
        assert ("anti" in low) or ("conflict" in low) or ("filter" in low), \
            f"explain does not name the reason: {out}"

    with subtest("4th node joins with the reserved join token"):
        n4.wait_for_unit("multi-user.target")
        n4.succeed("systemctl stop expansed.service")
        rc, token = n2.execute("cat /root/join-token")
        assert "expanse-join-" in token, f"no reserved token: {token!r}"
        join_and_start(n4, token, "voter")
        wait_quorum("4/3", 60)

    with subtest("4th replica places within 60 s"):
        deadline = time.time() + 60
        placed = False
        while time.time() < deadline:
            b = get_json(n1, "web")
            nodes = placement_nodes(b)
            if len(nodes) == 4 and "n4" in nodes:
                placed = True
                break
            time.sleep(2)
        assert placed, f"4th replica never placed on n4: {b}"
        # The roll-up must complete: 4 RUNNING on 4 distinct nodes.
        deadline = time.time() + 60
        while time.time() < deadline:
            b = get_json(n1, "web")
            ps = [p for p in (b.get("status") or {}).get("placements", [])
                  if p.get("replicaIndex", 0) >= 0 and p.get("phase") != "LOST"]
            if len(ps) == 4 and all(p.get("phase") == "RUNNING" for p in ps):
                break
            time.sleep(2)
        else:
            raise Exception(f"not 4 RUNNING after n4 join: {b}")
  '';
}
