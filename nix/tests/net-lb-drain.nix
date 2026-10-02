# §6 net-lb-drain (G5.13): sustained 50 req/s with HTTP keep-alive
# through the VIP during a rolling update; zero client-visible
# connection resets (drain: stop new work on the dying replica, let
# in-flight requests finish).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-net-lb-drain";

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
      virtualisation.memorySize = 2048;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 ];
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
      virtualisation.memorySize = 2048;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 ];
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
      virtualisation.memorySize = 2048;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 ];
    };
    # External client: named n9 so the name-sorted eth1 assignment
    # leaves 192.168.1.1-.3 for the cluster nodes.
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = with pkgs; [ curl jq ];
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./client-common.py}

    client = n9  # the external client VM
    ${builtins.readFile ./block-common.py}

    form("netlbdrain")
    wait_agent_ready(n1)
    wait_agent_ready(n2)
    wait_agent_ready(n3)


    def drain_manifest(greeting):
        return (
            "apiVersion: expanse.io/v1\nkind: Block\n"
            "metadata:\n  name: web\n  namespace: default\n"
            "spec:\n  type: web/whoami\n  replicas: 2\n"
            "  placement:\n    antiAffinity: ANTI_AFFINITY_NODE\n"
            "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
            f"  config:\n    port: 8080\n    greeting: \"{greeting}\"\n"
            "  network:\n    ports:\n"
            "      - name: http\n        port: 80\n        target_port: 8080\n"
            "        protocol: tcp\n        expose: EXPOSE_VIP\n"
            "    health_check:\n      readiness:\n        type: PROBE_TCP\n"
            "        port: 8080\n        period_seconds: 2\n"
        )

    with subtest("deploy whoami replicas=2 with expose: vip"):
        deploy(n1, "web", drain_manifest("v1-"))
        vip = wait_block_vip(n1, "web")

    with subtest("both replicas Running within 90 s"):
        b = wait_phase(n1, "web", ["RUNNING"], 90)
        nodes_ = placement_nodes(b)
        assert len(nodes_) == 2, f"want 2 distinct nodes, got {nodes_}: {b.get('status')}"

    with subtest("VIP serves v1 through the LB"):
        deadline = time.time() + 60
        out = ""
        while time.time() < deadline:
            rc, out = n9.execute(
                f"curl -s --connect-timeout 3 http://{vip}/ || true")
            if "v1-replica-" in out:
                break
            time.sleep(2)
        assert "v1-replica-" in out, f"VIP never served: {out!r}"

    # The load client: one curl invocation per second carrying 50 URLs —
    # curl keeps its connection(s) to the VIP alive across all 50 (HTTP
    # keep-alive), i.e. a sustained 50 req/s over persistent
    # connections. A TCP reset, hang or failed request makes the
    # invocation exit non-zero; "reset" in stderr identifies RSTs.
    client_script = "\n".join([
        "#!/bin/sh",
        "export PATH=/run/current-system/sw/bin:$PATH",
        "rm -f /tmp/dr.stat",
        "urls=$(printf 'http://" + vip + "/ %.0s' $(seq 1 50))",
        "while true; do",
        "  if err=$(curl -sS -o /dev/null --connect-timeout 3 --max-time 20 $urls 2>&1); then",
        "    echo SAMPLE_OK >> /tmp/dr.stat",
        "  else",
        "    case \"$err\" in",
        "      *reset*) echo RST >> /tmp/dr.stat ;;",
        "      *) echo SAMPLE_BAD:$err >> /tmp/dr.stat ;;",
        "    esac",
        "  fi",
        "  sleep 1",
        "done",
        "",
    ])
    n9.execute("rm -f /tmp/dr-script.sh")
    n9.execute("echo " + __import__("base64").b64encode(client_script.encode()).decode()
               + " | base64 -d > /tmp/dr-script.sh")
    n9.execute("chmod +x /tmp/dr-script.sh")

    with subtest("start the sustained keep-alive client (50 req/s)"):
        started = False
        n = "0"
        deadline = time.time() + 40
        while time.time() < deadline:
            if not started:
                rc, out = n9.execute(
                    "systemd-run --unit=drclient --description='drain client' "
                    "/tmp/dr-script.sh 2>&1 || true")
                if "Running as unit" in out:
                    started = True
                else:
                    print("systemd-run retry:", out)
            time.sleep(2)
            rc, n = n9.execute("grep -c SAMPLE_OK /tmp/dr.stat 2>/dev/null || true")
            if int(n.strip() or 0) >= 5:
                break
        assert int(n.strip() or 0) >= 5, f"client produced no samples: {n!r}"

    # Let the client accumulate samples before touching the block.
    time.sleep(6)

    with subtest("rolling update: greeting v1- → v2-"):
        deploy(n1, "web", drain_manifest("v2-"))

    with subtest("update completes: v2 served, both replicas RUNNING"):
        deadline = time.time() + 180
        done = False
        while time.time() < deadline:
            b = get_json(n1, "web")
            live = {i: ph for i, ph in replica_phases(b).items() if i >= 0}
            if len(live) == 2 and all(ph == "RUNNING" for ph in live.values()):
                try:
                    rc, out = n9.execute(f"curl -s --connect-timeout 3 http://{vip}/ || true")
                    if "v2-replica-" in out:
                        done = True
                        break
                except Exception:
                    pass
            time.sleep(2)
        assert done, f"rolling update did not complete: {b.get('status')}"

    with subtest("stop the client; zero resets, zero bad samples"):
        n9.execute("systemctl stop drclient.service 2>/dev/null || true")
        time.sleep(1)
        rst = int(n9.execute("grep -c RST /tmp/dr.stat 2>/dev/null || true")[1].strip() or 0)
        bad = int(n9.execute("grep -c SAMPLE_BAD /tmp/dr.stat 2>/dev/null || true")[1].strip() or 0)
        ok = int(n9.execute("grep -c SAMPLE_OK /tmp/dr.stat 2>/dev/null || true")[1].strip() or 0)
        print(f"drain samples: ok={ok} bad={bad} rst={rst}")
        assert rst == 0, f"{rst} connection resets during the rolling update (G5.13)"
        assert bad == 0, f"{bad} failed request batches during the rolling update"
        assert ok >= 10, f"client too quiet to prove availability: {ok} samples"
  '';
}
