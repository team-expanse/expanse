# §8 block-reschedule: deploy replicas=3; kill -9 the whole n3 VM;
# assert within 60 s a replacement is Running on n1 or n2 (anti-affinity
# relaxed) or Pending with a clear reason (antiAffinity=node); restore
# n3 and assert convergence (G4.6 reschedule half).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
  blockNode = {
    # Tight reconcile period: the 30 s deploy budget (G4.5) must cover
    # placement + bridge + follower reconcile + promotion.
    expanse.agent.period = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
  };
  mkNode = name: hostId: { ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = hostId;
    expanse.hostname = name;
    environment.systemPackages = with pkgs; [ openssl curl ];
    virtualisation.memorySize = 2048;
              # Tight reconcile period: the 30 s deploy budget (G4.5) must cover
            # placement + bridge + follower reconcile + promotion.
            expanse.agent.period = "5s";
            expanse.agent.blocksCatalog = ../blocks;
            expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
            environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    };
in
{
  name = "expanse-block-reschedule";

  nodes = {
    n1 = mkNode "n1" "00000001";
    n2 = mkNode "n2" "00000002";
    n3 = mkNode "n3" "00000003";
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}

    form("test")

    with subtest("deploy util/echo replicas=3 with antiAffinity=node"):
        deploy(n1, "web", echo_yaml("web", 3, 18082, "resched\n", antiaffinity=True))
        b = wait_phase(n1, "web", ["RUNNING"], 30)
        assert len(placement_nodes(b)) == 3, f"want 3 distinct nodes: {b}"

    with subtest("kill -9 the whole n3 VM"):
        # Sync ZFS first so the crash doesn't lose recent secret/key
        # writes (a -9 kill can drop <5 s of pool transactions).
        n3.crash()
        # The leader eventually notices (§4.8 unreachable grace) and the
        # reschedule pass evicts n3's placements.

    with subtest("within 60 s: n3's placement LOST, replacement Pending with a clear reason"):
        # antiAffinity=node: n1/n2 each hold a replica, so the §4.4
        # outcome is: n3's placement marked Lost/retired, and the
        # replacement stays Pending with the anti-affinity reason. What
        # must never happen: a third live replica squeezed onto n1/n2.
        deadline = time.time() + 60
        settled = False
        while time.time() < deadline:
            b = get_json(n1, "web")
            ps = (b.get("status") or {}).get("placements", [])
            live = [p for p in ps if p.get("replicaIndex", 0) >= 0
                    and p.get("phase") != "LOST"]
            lost = [p for p in ps if p.get("replicaIndex", 0) < 0
                    or p.get("phase") == "LOST"]
            nodes = set(p.get("nodeId") for p in live)
            # Anti-affinity invariant: at most one live replica per node
            # (holds at every instant, including before the grace period
            # expires).
            assert len(nodes) == len(live), f"anti-affinity violated: {ps}"
            # n3's placement stays RUNNING until the §4.4 grace expires;
            # settled = it left the live set and n1/n2 keep serving.
            n3_live = [p for p in live if p.get("nodeId") == "n3"]
            if not n3_live and len(live) == 2 \
                    and all(p.get("phase") == "RUNNING" for p in live) and lost:
                assert nodes <= {"n1", "n2"}, f"replica off the survivor set: {ps}"
                settled = True
                break
            time.sleep(2)
        assert settled, f"n3's placement never retired within 60 s: {b}"

        # The pending replacement must carry a clear reason: explain
        # names the anti-affinity/conflict verdict for the survivors.
        rc, out = n1.execute(f"expanse ctl block explain {SOCK} web 2>/dev/null || true")
        low = out.lower()
        assert "anti" in low or "conflict" in low, \
            f"explain does not name the reason: {out}"

    with subtest("restore n3; cluster converges"):
        # Restart the VM and rejoin the cluster; §4.9 re-enrollment via
        # the persisted store brings n3 back with its node record.
        n3.start()
        n3.wait_for_unit("multi-user.target")
        n3.succeed("systemctl start expansed.service")
        n3.wait_for_unit("expansed.service")
        rc, dbg = n1.execute(f"expanse ctl kv {SOCK} get /nodes/n3 || true")
        print(f"DBGN3: rc={rc} out={dbg!r}")
        rc, dbg = n1.execute(f"expanse ctl kv {SOCK} get /nodes/n3/status || true")
        print(f"DBGN3S: rc={rc} out={dbg!r}")
        # Give the membership + reschedule passes time; the block must
        # end fully Running (on any nodes, but ≤ 1 per node) within 60 s
        # of n3's return.
        deadline = time.time() + 60
        done = None
        while time.time() < deadline:
            b = get_json(n1, "web")
            nodes = placement_nodes(b)
            live = {i: ph for i, ph in replica_phases(b).items() if i >= 0}
            if len(nodes) == 3 and len(live) == 3 and all(ph == "RUNNING" for ph in live.values()):
                done = nodes
                break
            time.sleep(2)
        assert done, f"no convergence after n3 return: {b}"
  '';
}
