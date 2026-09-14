# §8 block-scale: scale util/echo 1→3→5→2→1 on a 3-node cluster,
# asserting the correct running count at every step and zero orphan
# units (no active expanse-block@* unit without a live placement).
# Anti-affinity is relaxed and the echo port is ephemeral (port 0) so
# multiple replicas may share a node.
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
    # Tight reconcile period: the budgets assume placement + bridge +
    # follower reconcile + promotion within ~30 s.
    expanse.agent.period = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
  };
in
{
  name = "expanse-block-scale";

  nodes = {
    n1 = mkNode "n1" "00000001";
    n2 = mkNode "n2" "00000002";
    n3 = mkNode "n3" "00000003";
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}

    form("test")
    MN = {"n1": n1, "n2": n2, "n3": n3}

    def live_units():
        """Active expanse-block@ units across the whole cluster."""
        total = 0
        for m in MN.values():
            rc, out = m.execute(
                "systemctl list-units 'expanse-block@*' --no-legend --no-pager "
                "2>/dev/null | grep -c active || true")
            total += int(out.strip() or 0)
        return total

    def live_placements(b):
        return {i: (p.get("nodeId"), p.get("phase"))
                for i, p in ((q.get("replicaIndex", 0), q)
                             for q in (b.get("status") or {}).get("placements", []))
                if i >= 0 and p.get("phase") not in ("LOST",)}

    def scale_to(n, timeout):
        """Apply the manifest at replicas=n, wait for convergence, then
        assert running count == n everywhere and no orphans."""
        deploy(n1, "web", echo_yaml("web", n, 0, "scale\n", antiaffinity=False))
        deadline = time.time() + timeout
        while time.time() < deadline:
            b = get_json(n1, "web")
            live = live_placements(b)
            running = {i for i, (_, ph) in live.items() if ph == "RUNNING"}
            if len(running) == n and len(live) == n:
                break
            time.sleep(2)
        assert len(running) == n and len(live) == n, \
            f"scale to {n} never converged: {b.get('status')}"
        # Every RUNNING placement has an active unit on its node…
        for i, (node, ph) in live.items():
            assert ph == "RUNNING", f"replica {i} not Running: {b.get('status')}"
            assert unit_running(MN[node], "default", "web", i), \
                f"replica {i} unit inactive on {node}"
        # …and there are no extra active units anywhere (orphans).
        # Retry: the retire is asynchronous — the bridge syncs the
        # deletions (10 s interval) and each agent's reconcile tick
        # (5 s) stops the units. A just-stopped unit can also linger as
        # "deactivating" in list-units for a moment.
        deadline = time.time() + 45
        units = live_units()
        while units != n and time.time() < deadline:
            time.sleep(1)
            units = live_units()
        assert units == n, f"unit count {units} != desired {n} (orphans?)"

    with subtest("scale 1"):
        scale_to(1, 45)
    with subtest("scale 1 → 3"):
        scale_to(3, 60)
    with subtest("scale 3 → 5"):
        scale_to(5, 60)
    with subtest("scale 5 → 2 (retire the surplus)"):
        scale_to(2, 90)
    with subtest("scale 2 → 1"):
        scale_to(1, 90)
  '';
}
