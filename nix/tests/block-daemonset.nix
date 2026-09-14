# §8 block-daemonset: deploy node-exporter-style daemonset (V6: one
# placement per eligible node — Ready, non-witness); assert 1 per node;
# add a node → it gets one automatically; cordon a node → the daemonset
# STAYS (daemonsets ignore cordon by default, §4.4).
{ self }:
{ pkgs, lib, ... }:
let
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
    # Replica endpoints are curled across nodes.
    networking.firewall.allowedTCPPortRanges = [{ from = 18000; to = 18999; }];
    # Tight reconcile period.
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
  };
in
{
  name = "expanse-block-daemonset";

  nodes = {
    n1 = mkNode "n1" "00000001";
    n2 = mkNode "n2" "00000002";
    n3 = mkNode "n3" "00000003";
    # The late joiner (starts idle; joins via the CLI after the first
    # assertions) must also be reachable for its new daemonset replica.
    n4 = mkNode "n4" "00000004";
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}

    # 4-token budget: n2/n3 consume two; the remaining one (persisted at
    # /root/join-token on n1) is for n4's late join.
    form("test", "voter", 4)

    with subtest("deploy daemonset, exactly 1 per node"):
        # V6: no replicas key. One replica per node → a fixed port is
        # conflict-free, and lets us curl each replica directly.
        deploy(n1, "ds", echo_yaml("ds", None, 18086, "ds\n",
                                   antiaffinity=False, strategy="DAEMONSET"))
        b = wait_phase(n1, "ds", ["RUNNING"], 30)
        nodes = placement_nodes(b)
        assert len(nodes) == 3, f"daemonset on {nodes}, want 1 per node: {b.get('status')}"

    with subtest("each replica serves on its own node"):
        for node in nodes:
            m = {"n1": n1, "n2": n2, "n3": n3}[node]
            echo_responds(m, 18086, "ds\n", node="localhost")

    with subtest("new node gets a replica automatically"):
        n4.wait_for_unit("multi-user.target")
        n4.succeed("systemctl stop expansed.service")
        rc, token = n2.execute("cat /root/join-token")
        join_and_start(n4, token.strip(), "voter")
        deadline = time.time() + 60
        b = None
        while time.time() < deadline:
            b = get_json(n1, "ds")
            if b is not None and len(placement_nodes(b)) == 4:
                break
            time.sleep(2)
        nodes = placement_nodes(b or {})
        st = (b or {}).get("status")
        assert len(nodes) == 4 and "n4" in nodes, \
            f"daemonset never extended to n4: {st}"
        echo_responds(n4, 18086, "ds\n", node="localhost")

    with subtest("cordon a node: daemonset stays (ignores cordon)"):
        # The node-lifecycle CLI opens the node's local bolt store, so
        # it cannot run while the daemon holds the lock (the Phase 03
        # stop → CLI → start dance). Cordon must run on the leader.
        n1.succeed("systemctl stop expansed.service")
        n1.succeed("expanse ctl node cordon n2")
        n1.succeed("systemctl start expansed.service")
        n1.wait_for_unit("expansed.service")
        # Convergence wait: the leader's own daemon restart makes n1's
        # readiness flap for a pass or two (its daemonset replica may
        # be culled and re-added), so settle first. The INVARIANT under
        # test: n2's cordoned-node placement SURVIVES — it must still
        # be there, RUNNING and serving, once everything converges.
        deadline = time.time() + 60
        ok = False
        b = None
        while time.time() < deadline:
            b = get_json(n2, "ds")
            idxs = {p.get("replicaIndex", 0): p for p in
                    ((b or {}).get("status") or {}).get("placements", [])
                    if p.get("phase") == "RUNNING"}
            if len(idxs) == 4 and any(p.get("nodeId") == "n2" for p in idxs.values()):
                ok = True
                break
            time.sleep(2)
        st = (b or {}).get("status")
        assert ok, f"daemonset lost its cordoned-node placement: {st}"
        echo_responds(n2, 18086, "ds\n", node="localhost")
  '';
}
