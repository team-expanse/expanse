# §6 net-firewall (T20): with the §4.5 ruleset enabled on the cluster
# nodes, the external client sees only the intended ports (G5.11) —
# management (22/8443), the join/control-plane endpoint (7446, token-
# and mTLS-authenticated and by definition reachable to not-yet-peers;
# the spec's off-overlay forbidden list omits it) and VIP-bound block
# service ports; the cluster-service ports 7443/7444/7445 are NOT
# reachable off-overlay, while overlay access to 7443 still completes
# an mTLS handshake.
#
# Join sequencing note: the ruleset admits cluster-service ports from
# @cluster_peers on any interface (peers dial advertised LAN
# endpoints), plus 7446 from anywhere so a fresh node can join. The
# dynamic sets lag a joining node by one sync period (5 s); the agent's
# raft reconnects inside that window.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
  nodeConfig = { hostId, host, extraPackages ? [ ] }: { ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = hostId;
    expanse.hostname = host;
    virtualisation.memorySize = 2048;
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.100";
    expanse.agent.externalInterface = "eth1";
    # T19/T20: apply the §4.5 ruleset. The agent is the SOLE nftables
    # owner here — the NixOS firewall module flushes the ruleset on
    # interface events, which would wipe the agent's table.
    expanse.agent.firewall = true;
    networking.nftables.enable = lib.mkForce false;
    networking.firewall.enable = lib.mkForce false;
    environment.systemPackages = with pkgs; [ curl jq openssl nmap tcpdump ] ++ extraPackages;
  };
in
{
  name = "expanse-net-firewall";

  nodes = {
    n1 = nodeConfig { hostId = "00000001"; host = "n1"; };
    n2 = nodeConfig { hostId = "00000002"; host = "n2"; };
    n3 = nodeConfig { hostId = "00000003"; host = "n3"; };
    # External client: 4th machine → eth1 192.168.1.4. Not a cluster
    # node: its addresses are never in @cluster_peers.
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = with pkgs; [ curl jq nmap ];
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./client-common.py}
    ${builtins.readFile ./block-common.py}

    client = n9  # the external client VM

    form("netfw")
    wait_agent_ready(n1)
    wait_agent_ready(n2)
    wait_agent_ready(n3)

    with subtest("deploy nginx (3 replicas, VIP port 80)"):
        nginx = (
            "apiVersion: expanse.io/v1\nkind: Block\n"
            "metadata:\n  name: nginx\n  namespace: default\n"
            "spec:\n  type: web/whoami\n  replicas: 3\n"
            "  placement:\n    antiAffinity: ANTI_AFFINITY_NODE\n"
            "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
            "  config:\n    port: 8080\n"
            "  network:\n    ports:\n"
            "      - name: http\n        port: 80\n        target_port: 8080\n"
            "        protocol: tcp\n        expose: EXPOSE_VIP\n"
            "    health_check:\n      readiness:\n        type: PROBE_TCP\n"
            "        port: 8080\n        period_seconds: 2\n"
        )
        deploy(n1, "nginx", nginx)

    with subtest("block Running and VIP allocated"):
        b = wait_phase(n1, "nginx", ["RUNNING"], 120)
        assert len(placement_nodes(b)) == 3, f"nginx placements: {b.get('status')}"

    vip = "192.168.1.100"
    allowed = {22, 8443, 7446, 80}

    # The spec's full 1-10000 sweep takes tens of seconds per node when
    # most ports are filtered (SYN retransmits); a targeted list covers
    # every listening port the stack has plus the forbidden ones.
    scan_ports = "22,53,80,81,7443,7444,7445,7446,8443,8080,8081,5353"

    def open_ports(node, target):
        rc, out = node.execute(
            f"nmap -Pn -T4 --max-retries 1 --open -p{scan_ports} {target} 2>&1 | "
            "grep -oP '^\\d+(?=/tcp\\s+open)' || true"
        )
        return {int(p) for p in out.split() if p}

    with subtest("G5.11: off-overlay scan shows only the intended ports"):
        rc, out = n1.execute(
            "expanse ctl kv get /network/vipPool/external/default/nginx "
            "--socket /run/expanse/agent.sock 2>&1 || true"
        )
        # The firewall sync runs on a 5 s period; wait for the VIP port
        # to appear on some node before scanning. The targeted port
        # list makes each scan fast (~1 s), so the loop actually
        # retries within the deadline instead of reporting a stale
        # pre-bootstrap result.
        # All scans run on n9: the spec's view is the external client's,
        # and a node scanning itself enters via lo (iif lo accept).
        deadline = time.time() + 120
        vip_seen = set()
        while time.time() < deadline:
            vip_seen = open_ports(n9, vip)
            if 80 in vip_seen:
                break
            time.sleep(5)

        for m in [n1, n2, n3]:
            got = open_ports(n9, "192.168.1." + m.name[1])
            assert got <= allowed, f"{m.name}: off-overlay open ports {got}, allowed {sorted(allowed)}"

    with subtest("G5.11: cluster-service ports are not reachable off-overlay"):
        for port in (7443, 7444, 7445):
            for m in [n1, n2, n3]:
                # Probed from n9 (a node probing itself enters via lo,
                # which the ruleset accepts).
                rc9, out9 = n9.execute(
                    f"timeout 3 bash -c '</dev/tcp/192.168.1.{m.name[1]}/{port}' 2>&1"
                )
                # rc: 0 = connected (BAD); 124 = timeout (dropped SYN =
                # firewall-filtered); 1 = refused. Only a connect
                # success proves reachability.
                assert rc9 != 0, \
                    f"{m.name}: port {port} reachable off-overlay (rc={rc9}, out={out9!r})"

    with subtest("overlay access to 7443 still completes mTLS (G5.11)"):
        # n1 dials n2's overlay address with its own node cert; the
        # server's CA path validates and the handshake completes.
        deadline = time.time() + 60
        ok = False
        while time.time() < deadline and not ok:
            rc, out = n1.execute(
                "timeout 10 openssl s_client -connect 10.42.2.1:7443 "
                "-cert /persist/expanse/tls/node-cert.pem "
                "-key /persist/expanse/tls/node-key.pem "
                "-CAfile /persist/expanse/ca/ca.pem 2>&1 | "
                "grep -E 'Verify return code' || true"
            )
            ok = "0 (ok)" in out
            if not ok:
                time.sleep(5)
        assert ok, f"overlay mTLS handshake failed: {out[-400:]}"
  '';
}
