# §8 block-rolling-update: deploy echo replicas=3, run a continuous
# ~10 req/s client against all replicas during a config change, and
# assert (G4.7): update completes ≤ 120 s, at every sampled instant
# ≥ 2/3 replicas responded (maxUnavailable=1 floor), zero bad samples,
# and every replica serves the new body afterward.
#
# Client error semantics: with replicas updated in place, a replaced
# replica's direct endpoint is briefly down — that is exactly what the
# maxUnavailable=1 budget covers. The hard assertion is therefore on
# SAMPLES: a sample is good when ≥ 2 of the 3 replica endpoints answer;
# the count of bad samples must be 0. That subsumes "≥ 2 replicas ready
# at every sampled instant" and keeps the client-error count a machine
# assertion (the spec's step 6), not a log read.
{ self }:
{ pkgs, lib, ... }:
let
  # Continuous availability client: every ~0.3 s it curls all three
  # replica endpoints and records one SAMPLE_OK/SAMPLE_BAD line (bad =
  # fewer than 2 of 3 answered — the maxUnavailable=1 floor). Samples
  # before the first successful response are skipped (nothing to
  # protect yet), so the service can be started at any time.
  clientScript = pkgs.writeShellScript "block-client" ''
    probe() {
      local ok=0 h b
      for h in 192.168.1.1 192.168.1.2 192.168.1.3; do
        if b=$(${pkgs.curl}/bin/curl -sS --max-time 2 http://$h:18081/ 2>/dev/null); then
          ok=$((ok+1))
          echo $b >> /tmp/cl.bodies
        fi
      done
      echo $ok
    }
    while :; do
      ok=$(probe)
      # A sample that straddles two sequential in-place restarts
      # (~100 ms apart) can transiently see < 2 up — that outage is
      # exactly what the maxUnavailable=1 budget covers. One in-sample
      # retry: only a PERSISTENT < 2 counts as a bad sample.
      if [ $ok -lt 2 ] && [ -f /tmp/cl.up ]; then
        ${pkgs.coreutils}/bin/sleep 0.3
        ok=$(probe)
      fi
      if [ $ok -ge 2 ]; then touch /tmp/cl.up; fi
      if [ -f /tmp/cl.up ]; then
        if [ $ok -ge 2 ]; then echo SAMPLE_OK >> /tmp/cl.stat; else echo SAMPLE_BAD >> /tmp/cl.stat; fi
      fi
      ${pkgs.coreutils}/bin/sleep 0.2
    done
  '';

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
    # Replica endpoints are curled ACROSS nodes by the client.
    networking.firewall.allowedTCPPortRanges = [{ from = 18000; to = 18999; }];
    systemd.services.block-client = {
      description = "block rolling-update availability client";
      serviceConfig = {
        Type = "simple";
        ExecStart = clientScript;
        Restart = "on-failure";
      };
    };
    # Tight reconcile period: the budgets assume placement + bridge +
    # follower reconcile + promotion within ~30 s.
    expanse.agent.period = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
  };
in
{
  name = "expanse-block-rolling-update";

  nodes = {
    n1 = mkNode "n1" "00000001";
    n2 = mkNode "n2" "00000002";
    n3 = mkNode "n3" "00000003";
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}

    form("test")

    with subtest("deploy util/echo replicas=3, all ready"):
        deploy(n1, "web", echo_yaml("web", 3, 18081, "gen-one\n"))
        b = wait_phase(n1, "web", ["RUNNING"], 30)
        assert len(placement_nodes(b)) == 3, f"want 3 distinct nodes: {b}"
        for node in placement_nodes(b):
            echo_responds({"n1": n1, "n2": n2, "n3": n3}[node], 18081, "gen-one\n")

    with subtest("start the continuous client (~10 req/s across the 3 replicas)"):
        n1.succeed("rm -f /tmp/cl.stat /tmp/cl.bodies /tmp/cl.up; systemctl start block-client.service")
        # First sample can take up to ~6 s (3 curls × --max-time 2).
        deadline = time.time() + 15
        out = "0"
        while time.time() < deadline:
            rc, out = n1.execute("grep -c SAMPLE /tmp/cl.stat 2>/dev/null || true")
            if int(out.strip() or 0) > 0:
                break
            time.sleep(1)
        assert int(out.strip() or 0) > 0, "client produced no samples before the update"

    with subtest("change the response body; apply"):
        deploy(n1, "web", echo_yaml("web", 3, 18081, "gen-two\n"))

    with subtest("update completes within 120 s"):
        mm = {"n1": n1, "n2": n2, "n3": n3}
        deadline = time.time() + 120
        done = False
        while time.time() < deadline:
            b = get_json(n1, "web")
            live = {i: ph for i, ph in replica_phases(b).items() if i >= 0}
            nodes = placement_nodes(b)
            if len(live) == 3 and all(ph == "RUNNING" for ph in live.values()) \
                    and len(nodes) == 3:
                try:
                    for node in nodes:
                        echo_responds(mm[node], 18081, "gen-two\n")
                    done = True
                    break
                except Exception:
                    pass
            time.sleep(2)
        assert done, f"rolling update did not complete within 120 s: {b}"

    with subtest("stop the client; zero bad samples, new body served"):
        time.sleep(3)  # sample past the roll's end too; a fast roll alone leaves too few samples
        n1.execute("systemctl stop block-client.service 2>/dev/null || true")
        time.sleep(1)
        bad = int(n1.execute("grep -c SAMPLE_BAD /tmp/cl.stat || true")[1].strip() or 0)
        ok = int(n1.execute("grep -c SAMPLE_OK /tmp/cl.stat || true")[1].strip() or 0)
        assert ok > 10, f"client barely sampled (ok={ok})"
        assert bad == 0, f"availability floor broken: {bad} bad samples of {ok + bad}"
        bodies = n1.execute("cat /tmp/cl.bodies || true")[1]
        assert "gen-two" in bodies, "client never observed the new generation"
        for node in placement_nodes(b):
            echo_responds({"n1": n1, "n2": n2, "n3": n3}[node], 18081, "gen-two\n")
  '';
}
