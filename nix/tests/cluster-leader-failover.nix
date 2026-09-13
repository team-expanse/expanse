# §6 cluster-leader-failover: kill the leader's daemon, a new leader
# must be elected within 10 s, writes must work, the old leader must
# rejoin as follower and catch up within 30 s, and nothing acked is lost
# (G3.5, G3.7).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-cluster-leader-failover";

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

    form("failover")

    with subtest("canary write before the kill"):
        rc, out = kv(n1, "put /failover/canary pre-kill")
        assert rc == 0, out

    with subtest("stop the leader; new leader within 10 s"):
        rep = status(n1)
        leader = leader_of(rep)
        assert leader, f"no leader in: {rep}"
        print(f"current leader: {leader}")
        t0 = time.time()
        machine = {"n1": n1, "n2": n2, "n3": n3}[leader]
        machine.succeed("systemctl stop expansed.service")

        survivors = [m for m in [n1, n2, n3] if m != machine]
        new_leader = ""
        deadline = t0 + 10
        while time.time() < deadline:
            cand = leader_of(status(survivors[0]))
            if cand and cand != leader:
                new_leader = cand
                break
            time.sleep(0.5)
        assert new_leader, f"no new leader within 10 s (was {leader})"
        took = time.time() - t0
        print(f"new leader {new_leader} elected in {took:.1f}s")
        assert took <= 10

    with subtest("writes succeed after failover, canary intact"):
        survivor = survivors[0]
        rc, out = kv(survivor, "put /failover/post post-kill")
        assert rc == 0, f"write failed after failover: {out}"
        for m in [n1, n2, n3]:
            if m == machine:
                continue  # still down
            rc, out = kv(m, "get /failover/canary")
            assert out.strip() == "pre-kill", f"{m.name}: canary lost: {out}"

    with subtest("old leader rejoins as follower, catches up within 30 s"):
        machine.succeed("systemctl start expansed.service")
        machine.wait_for_unit("expansed.service")
        deadline = time.time() + 30
        caught_up = False
        while time.time() < deadline:
            rc, out = kv(machine, "get /failover/post")
            if rc == 0 and out.strip() == "post-kill":
                caught_up = True
                break
            time.sleep(1)
        assert caught_up, f"{leader} did not catch up within 30 s"
        # Back to full health.
        rep = wait_quorum("3/2", 15)
        # The returned node must not lead again (no leadership snap-back).
        assert leader_of(rep) != leader, f"leadership snapped back to {leader}"

    with subtest("zero data loss of acked writes"):
        for m in [n1, n2, n3]:
            rc, out = kv(m, "get /failover/canary")
            assert out.strip() == "pre-kill", f"{m.name}: canary lost: {out}"
            rc, out = kv(m, "get /failover/post")
            assert out.strip() == "post-kill", f"{m.name}: post-kill write lost: {out}"
  '';
}
