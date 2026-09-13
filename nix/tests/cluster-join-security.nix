# §6 cluster-join-security: bad/expired/consumed/raced tokens are
# rejected; a self-signed client cert cannot complete the :7443 TLS
# handshake; and 100 rotating writes leave zero cleartext on the wire
# (G3.8).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-cluster-join-security";

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
      environment.systemPackages = [ pkgs.openssl pkgs.tcpdump ];
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
      environment.systemPackages = [ pkgs.openssl ];
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
      environment.systemPackages = [ pkgs.openssl ];
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}

    def mint_token(ttl="--ttl 15m", uses="--uses 1"):
        """Mint a token via n1's CLI. Only valid while n1's daemon is
        stopped AND n1 is still the sole voter (its CLI proposes
        locally, so it must be the leader) — see the bootstrap below."""
        rc, out = n1.execute(
            f"expanse cluster token --data-dir /persist/expanse create {ttl} {uses} 2>&1"
        )
        m = re.search(r"expanse-join-[A-Za-z0-9_-]+", out)
        assert m, f"no token minted: {out}"
        return m.group(0)

    # Custom bootstrap: ALL tokens are minted up front, while n1's
    # daemon is down and n1 is the only voter. Mid-test mints are
    # impossible: the CLI owns the bolt lock (daemon must stop) and a
    # stopped leader's CLI is just a follower (propose: not leader).
    start_all()
    for m in [n1, n2, n3]:
        m.wait_for_unit("multi-user.target")
        m.succeed("systemctl stop expansed.service")

    n1.succeed(
        "expanse cluster init --data-dir /persist/expanse "
        "--name join-security --node-id n1 --advertise-addr 192.168.1.1:7444 --expect 3"
    )
    tok_form = mint_token(uses="--uses 3")
    tok_expired = mint_token("--ttl 2s")
    tok_used = mint_token()
    tok_race = mint_token()

    n1.succeed("systemctl start expansed.service")
    n1.wait_for_unit("expansed.service")
    join_and_start(n2, tok_form)
    join_and_start(n3, tok_form)
    wait_quorum("3/2", 60)

    with subtest("join with an invalid token is rejected"):
        rc, out = n2.execute(
            "expanse cluster join --data-dir /tmp/jinv --node-id n4 "
            "--address 192.168.1.1:7446 --token expanse-join-bogus "
            "--bind-addr 0.0.0.0:0 2>&1"
        )
        assert rc != 0, f"invalid token accepted: {out}"
        assert "token" in out.lower(), out
        assert not has_node(status(n1), "n4"), "invalid-token join created a node record"

    with subtest("join with an expired token is rejected"):
        time.sleep(3)  # tok_expired (2s TTL) is long gone
        rc, out = n2.execute(
            "expanse cluster join --data-dir /tmp/jexp --node-id n4 "
            f"--address 192.168.1.1:7446 --token {tok_expired} "
            "--bind-addr 0.0.0.0:0 2>&1"
        )
        assert rc != 0, f"expired token accepted: {out}"
        assert "expir" in out.lower(), out

    with subtest("self-signed client cert is rejected on :7443 (G3.8)"):
        n2.succeed(
            "openssl req -x509 -newkey rsa:2048 -keyout /tmp/self.key "
            "-out /tmp/self.pem -days 1 -nodes -subj /CN=n4 2>/dev/null"
        )
        # TLS 1.3 lets the client's side of the handshake complete
        # before the server's cert verdict arrives, so s_client always
        # prints a session. Send the HTTP/2 preface (force the client
        # to use the connection) and assert the server's TLS alert
        # comes back — the cert was rejected.
        rc, out = n2.execute(
            "( printf 'PRI * HTTP/2.0\\r\\n\\r\\nSM\\r\\n\\r\\n'; sleep 5 ) | "
            "timeout 10 openssl s_client -connect 192.168.1.1:7443 "
            "-cert /tmp/self.pem -key /tmp/self.key 2>&1"
        )
        assert "alert" in out.lower(), \
            f"server did not reject the self-signed client cert: {out[-400:]}"

    with subtest("zero cleartext on the wire during 60 rotating writes (G3.8)"):
        n1.execute("rm -f /tmp/d.pcap")
        n1.execute("nohup tcpdump -i eth1 -w /tmp/d.pcap port 7443 > /tmp/tcpdump.out 2>&1 &")
        time.sleep(2)
        ms = [n1, n2, n3]
        for i in range(60):
            rc, out = kv(ms[i % 3], f"put /sec/k{i} secretvalue-{i}")
            assert rc == 0, out
        n1.succeed("pkill tcpdump")
        time.sleep(1)
        rc, out = n1.execute("tcpdump -r /tmp/d.pcap 2>/dev/null | wc -l")
        n_pkts = int(out.strip() or 0)
        assert n_pkts > 0, "no packets captured — dump misconfigured (writes were local?)"
        rc, out = n1.execute("tcpdump -r /tmp/d.pcap -A 2>/dev/null | grep -c secretvalue || true")
        assert out.strip() == "0", f"cleartext leaked on the wire: {out}"

    with subtest("consumed single-use token is rejected"):
        # The joining CLI opens its own raft transport — a distinct
        # port, since n2's daemon holds :7444. The record's raft addr
        # points at that CLI port (n4 never runs a daemon here).
        rc, out = n2.execute(
            "expanse cluster join --data-dir /tmp/j4 --node-id n4 "
            f"--address 192.168.1.1:7446 --token {tok_used} "
            "--bind-addr 0.0.0.0:18981 --advertise-addr 192.168.1.2:18981 2>&1"
        )
        assert rc == 0, f"first join with a fresh single-use token failed: {out}"
        rc, out = n3.execute(
            "expanse cluster join --data-dir /tmp/j5 --node-id n5 "
            f"--address 192.168.1.1:7446 --token {tok_used} "
            "--advertise-addr 192.168.1.3:7444 --bind-addr 0.0.0.0:0 2>&1"
        )
        assert rc != 0, f"consumed token accepted: {out}"

    with subtest("two nodes racing one single-use token: exactly one succeeds"):
        join_cmd = lambda nid, port: (
            "expanse cluster join --data-dir /tmp/jrace-" + nid + " --node-id " + nid +
            " --address 192.168.1.1:7446 --token " + tok_race +
            f" --bind-addr 0.0.0.0:{port} --advertise-addr 192.168.1.2:{port}"
        )
        # Both racers from n2's VM (its daemon keeps :7444; the CLI
        # transports get their own ports) — a true simultaneous race.
        # Detached with setsid: the test driver tears down its shell's
        # process group when execute returns, which orphans the join
        # CLI before it can write its rc file.
        n2.execute(
            f"setsid nohup sh -c '{join_cmd('n4r', 18982)} > /tmp/race2.log 2>&1; echo $? > /tmp/race2.rc' >/dev/null 2>&1 &"
            f"setsid nohup sh -c '{join_cmd('n5r', 18983)} > /tmp/race3.log 2>&1; echo $? > /tmp/race3.rc' >/dev/null 2>&1 &"
        )
        deadline = time.time() + 60
        while time.time() < deadline:
            _, r2 = n2.execute("cat /tmp/race2.rc 2>/dev/null || echo pending")
            _, r3 = n2.execute("cat /tmp/race3.rc 2>/dev/null || echo pending")
            if r2.strip() != "pending" and r3.strip() != "pending":
                break
            time.sleep(1)
        rc, logs = n2.execute("cat /tmp/race2.log /tmp/race3.log 2>/dev/null; ps aux | grep -F 'cluster join' | grep -v grep")
        print(f"race diagnostics: {logs}")
        assert r2.strip() != "pending" and r3.strip() != "pending", \
            f"race did not settle: n2={r2} n3={r3}"
        rcs = sorted([int(r2.strip()), int(r3.strip())])
        assert rcs == [0, 1], f"expected exactly one winner: {rcs}"
        s = status(n1)
        got = [nid for nid in ("n4r", "n5r") if has_node(s, nid)]
        assert len(got) == 1, f"expected exactly one node record from the race: {got}"

    with subtest("cluster still healthy after all the rejection attempts"):
        rc, out = kv(n1, "put /sec/final ok")
        assert rc == 0, out
        s = status(n1)
        assert leaders(s), f"no leader after security subtests: {s}"
  '';
}
