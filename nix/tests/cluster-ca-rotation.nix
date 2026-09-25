# Phase 10 X2: a running 3-node cluster rotates to a fresh root CA with
# zero downtime (quorum and reads/writes keep working throughout, no
# node evicted), every node's renewal loop reissues its own cert onto
# the new CA with no listener restart, and the old CA is retired at the
# end. `expanse cluster ca rotate/complete` reopen the local raft store
# directly (like the pre-existing `token create`), so the node running
# them briefly stops its own daemon -- the other two keep the cluster up
# the whole time, exactly as cluster-node-loss.nix already treats as
# normal. `renewalPeriod` is set short so the test doesn't wait out a
# real 30-day cert lifetime.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
  node = { hostname, hostId }: { ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.agent.renewalPeriod = "3s";
    expanse.hostId = hostId;
    expanse.hostname = hostname;
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-cluster-ca-rotation";

  nodes = {
    n1 = node { hostname = "n1"; hostId = "00000001"; };
    n2 = node { hostname = "n2"; hostId = "00000002"; };
    n3 = node { hostname = "n3"; hostId = "00000003"; };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}

    def retry_cli(m, cmd, timeout=60):
        """Re-run a just-restarted node's raft-store-owning CLI command
        (like `token create`) until it succeeds: reopening the raft
        transport and its leader-forwarding dial from cold needs a
        moment to reconnect before a linearizable read/write goes
        through -- any `unavailable`-kind error during that window is
        exactly the transient condition to retry through, not fail on."""
        deadline = time.time() + timeout
        out = ""
        while time.time() < deadline:
            rc, out = m.execute(f"{cmd} 2>&1")
            if rc == 0:
                return out
            if "unavailable" not in out.lower():
                raise AssertionError(f"{cmd} failed: {out}")
            time.sleep(2)
        raise AssertionError(f"{cmd} never succeeded within {timeout}s: {out}")

    form("ca-rotation")

    with subtest("baseline: a write survives, capture the pre-rotation CA trust record"):
        rc, out = kv(n1, "put /rotate/marker before")
        assert rc == 0, out
        rc, orig_trust = n1.execute(
            "expanse ctl kv get /cluster/ca/trust --socket /run/expanse/agent.sock"
        )
        assert rc == 0, orig_trust
        assert '"outgoing"' not in orig_trust, f"already rotating before the test started: {orig_trust}"

    with subtest("start rotation -- n3's own daemon briefly down to run the CLI, like `token create`"):
        n3.succeed("systemctl stop expansed.service")
        retry_cli(n3, "expanse cluster ca rotate --data-dir /persist/expanse --node-id n3")

    with subtest("zero downtime while n3 is down for the trigger: n1/n2 keep serving reads and writes"):
        for i in range(5):
            rc, out = kv(n1, f"put /rotate/during {i}")
            assert rc == 0, out
            rc, out = kv(n2, "get /rotate/marker")
            assert rc == 0 and out.strip() == "before", out
            time.sleep(1)

    with subtest("n3 rejoins -- no node evicted by the rotation trigger"):
        n3.succeed("systemctl start expansed.service")
        n3.wait_for_unit("expansed.service")
        wait_agent_ready(n3)
        rep = wait_quorum("3/2", 30)
        for nid in ("n1", "n2", "n3"):
            assert has_node(rep, nid), f"{nid} missing after the rotation trigger: {rep}"

    with subtest("every node's renewal loop reissues onto the new primary -- no listener restart"):
        deadline = time.time() + 60
        out = ""
        converged = False
        while time.time() < deadline:
            rc, out = n1.execute("expanse ctl kv list /nodes/ --socket /run/expanse/agent.sock")
            assert rc == 0, out
            fps = re.findall(r'"ca_fp":"([0-9a-f]+)"', out)
            if len(fps) == 3 and len(set(fps)) == 1:
                converged = True
                break
            time.sleep(2)
        assert converged, f"nodes never converged on one CA fingerprint within 60s: {out}"

        # Nothing was ever stopped to pick up the new cert -- proves the
        # dynamic (no-restart) TLS reload, not just that a restart would
        # have worked.
        for m in [n1, n2, n3]:
            m.succeed("systemctl is-active expansed.service")

    with subtest("zero downtime throughout renewal: writes/reads still succeed on every node"):
        for i, m in enumerate([n1, n2, n3]):
            rc, out = kv(m, f"put /rotate/post{i} ok")
            assert rc == 0, out
            rc, out = kv(m, f"get /rotate/post{i}")
            assert rc == 0 and out.strip() == "ok", out

    with subtest("complete rotation: old CA retired, no node evicted"):
        n3.succeed("systemctl stop expansed.service")
        retry_cli(n3, "expanse cluster ca complete --data-dir /persist/expanse --node-id n3")
        n3.succeed("systemctl start expansed.service")
        n3.wait_for_unit("expansed.service")
        wait_agent_ready(n3)

        rep = wait_quorum("3/2", 30)
        for nid in ("n1", "n2", "n3"):
            assert has_node(rep, nid), f"{nid} missing after `ca complete`: {rep}"

        rc, new_trust = n1.execute(
            "expanse ctl kv get /cluster/ca/trust --socket /run/expanse/agent.sock"
        )
        assert rc == 0, new_trust
        assert '"outgoing"' not in new_trust, f"outgoing CA still present after complete: {new_trust}"
        assert new_trust != orig_trust, "CA trust record is unchanged from before the rotation"

    with subtest("final sanity: reads/writes across every node after the full rotation"):
        rc, out = kv(n1, "get /rotate/marker")
        assert rc == 0 and out.strip() == "before", out
        rc, out = kv(n2, "put /rotate/final done")
        assert rc == 0, out
        rc, out = kv(n3, "get /rotate/final")
        assert rc == 0 and out.strip() == "done", out

    print("CA rotation: zero downtime, no node evicted, old CA retired")
  '';
}
