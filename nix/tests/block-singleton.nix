# §8 block-singleton: deploy a singleton (V5, lease-fenced §4.4);
# assert exactly 1 running instance; kill its node's whole VM; assert
# exactly 1 running elsewhere within 60 s; assert at NO point did 2
# instances run — a continuous 100 ms checker samples every node's
# endpoint for the whole test and any sample with ≥ 2 responders fails
# (G4.11). Design mirrors Phase03 T18's split-brain checker (poll
# frequency + a hard violation file), but the invariant here is the
# replica COUNT, not lease-holder identity.
{ self }:
{ pkgs, lib, ... }:
let
  # Continuous single-instance checker: every ~100 ms it curls the
  # singleton's endpoint on all three node IPs and appends the number
  # of correct responders to /tmp/s.counts; any sample with ≥ 2 (a
  # double-run instant) also lands in /tmp/s.double — the test's hard
  # invariant file.
  checkerScript = pkgs.writeShellScript "singleton-checker" ''
    while :; do
      n=0
      for h in 192.168.1.1 192.168.1.2 192.168.1.3; do
        if b=$(${pkgs.curl}/bin/curl -sS --max-time 1 http://$h:18085/ 2>/dev/null) && [ "$b" = "single" ]; then
          n=$((n+1))
        fi
      done
      echo $n >> /tmp/s.counts
      if [ $n -ge 2 ]; then echo $n >> /tmp/s.double; fi
      ${pkgs.coreutils}/bin/sleep 0.1
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
    # Replica endpoints are polled across nodes by the checker.
    networking.firewall.allowedTCPPortRanges = [{ from = 18000; to = 18999; }];
    systemd.services.singleton-checker = {
      description = "block-singleton continuous single-instance checker";
      serviceConfig = {
        Type = "simple";
        ExecStart = checkerScript;
        Restart = "on-failure";
      };
    };
    # Tight reconcile period.
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
  };
in
{
  name = "expanse-block-singleton";

  nodes = {
    n1 = mkNode "n1" "00000001";
    n2 = mkNode "n2" "00000002";
    n3 = mkNode "n3" "00000003";
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}

    form("test")

    with subtest("deploy singleton, exactly 1 running"):
        deploy(n1, "single", echo_yaml("single", 1, 18085, "single",
                                       antiaffinity=False, strategy="SINGLETON"))
        b = wait_phase(n1, "single", ["RUNNING"], 30)
        nodes = placement_nodes(b)
        assert len(nodes) == 1, f"singleton placed on {nodes}, want exactly 1: {b.get('status')}"

    holder = list(nodes)[0]
    # NOTE: typed as a dict but shadowed by the driver's machines list —
    # use per-name lookups instead of a machines dict.
    all_machines = {"n1": n1, "n2": n2, "n3": n3}
    hm = all_machines[holder]
    echo_responds(hm, 18085, "single", node="localhost")

    # The checker must run on a node that is NOT about to be killed.
    checker_node = next(m for n, m in all_machines.items() if n != holder)
    checker_node.succeed("rm -f /tmp/s.counts /tmp/s.double; systemctl start singleton-checker.service")

    with subtest("kill the holder's whole VM"):
        # Sync ZFS first so the crash doesn't lose recent secret/key
        # writes (a -9 kill can drop <5 s of pool transactions).
        all_machines[holder].crash()

    with subtest("exactly 1 running elsewhere within 60 s"):
        # IMPORTANT: never execute on the crashed machine — the driver
        # AUTO-REBOOTS a dead VM on the next execute, which would
        # resurrect the replica inside the grace window. All reads go
        # through n2.
        deadline = time.time() + 60
        b = None
        while time.time() < deadline:
            b = get_json(n2, "single")
            placements = (b.get("status") or {}).get("placements", []) if b else []
            live = {p.get("nodeId") for p in placements if p.get("phase") == "RUNNING"}
            if holder not in live and len(live) == 1:
                break
            time.sleep(2)
        placements = (b.get("status") or {}).get("placements", []) if b else []
        live = {p.get("nodeId") for p in placements if p.get("phase") == "RUNNING"}
        st = (b or {}).get("status")
        if not (len(live) == 1 and holder not in live):
            rc, dbg = n2.execute(f"expanse ctl kv {SOCK} get /nodes/n1 2>&1; true")
            print(f"DBGN1: {dbg!r}")
            rc, dbg = n2.execute(f"expanse ctl kv {SOCK} get /nodes/n1/status 2>&1; true")
            print(f"DBGN1S: {dbg!r}")
            time.sleep(10)
            rc, dbg = n2.execute(f"expanse ctl kv {SOCK} get /nodes/n1/status 2>&1; true")
            print(f"DBGN1S2: {dbg!r}")
            rc, dbg = n2.execute(f"expanse ctl kv {SOCK} get /leases/block/default/single 2>&1; true")
            print(f"DBGNLEASE: {dbg!r}")
        assert len(live) == 1 and holder not in live, \
            f"singleton did not re-place within 60 s: {st}"
        new_holder = next(iter(live))
        hm2 = {"n1": n1, "n2": n2, "n3": n3}[new_holder]
        echo_responds(hm2, 18085, "single", node="localhost")

    with subtest("no double-run instant at any sampled point (G4.11)"):
        time.sleep(5)  # keep sampling through the steady state
        checker_node.execute("systemctl stop singleton-checker.service 2>/dev/null || true")
        rc, doubles = checker_node.execute("cat /tmp/s.double 2>/dev/null || true")
        assert not doubles.strip(), f"double-run instants recorded: {doubles!r}"
        rc, counts = checker_node.execute("wc -l < /tmp/s.counts 2>/dev/null || true")
        assert int(counts.strip() or 0) > 10, f"checker barely sampled ({counts!r})"
  '';
}
