# §6 cluster-partition: nftables-isolate n3 from n1,n2 — n3 reports
# degraded and rejects writes with "unavailable" while n1/n2 keep
# serving; the lease n3 held is self-released before its TTL and can
# be taken over only after it (guard band, §4.3); healing rejoins n3
# with zero divergence (G3.9).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-cluster-partition";

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

    form("partition")

    TTL = 15
    TTL_NS = TTL * 10**9

    def wait_for_event(m, logfile, phase, timeout=15):
        deadline = time.time() + timeout
        out = ""
        while time.time() < deadline:
            rc, out = m.execute(f"grep -c '{phase}' {logfile} || true")
            if out.strip().isdigit() and int(out.strip()) > 0:
                return True
            time.sleep(1)
        return False

    with subtest("seed state and take a lease on n3"):
        for i in range(3):
            rc, out = kv(n1, f"put /part/k{i} v{i}")
            assert rc == 0, out
        n3.succeed(
            "nohup expanse ctl lease hold /part/lease --ttl 15s "
            "--timeout 300s --socket /run/expanse/agent.sock > /tmp/lease-n3.log 2>&1 &"
        )
        assert wait_for_event(n3, "/tmp/lease-n3.log", "acquired"), "n3 never acquired the lease"
        rc, out = n1.execute("expanse ctl lease holder /part/lease --socket /run/expanse/agent.sock 2>&1")
        assert "holder:    n3" in out, f"n1 does not see n3 holding: {out}"

    with subtest("partition n3 from n1,n2 (nftables drop both directions)"):
        n3.succeed(
            "nft add table ip exppart; "
            "nft add chain ip exppart output '{ type filter hook output priority 0; }'; "
            "nft add rule ip exppart output ip daddr 192.168.1.1 drop; "
            "nft add rule ip exppart output ip daddr 192.168.1.2 drop; "
            "nft add chain ip exppart input '{ type filter hook input priority 0; }'; "
            "nft add rule ip exppart input ip saddr 192.168.1.1 drop; "
            "nft add rule ip exppart input ip saddr 192.168.1.2 drop"
        )

    with subtest("n3 degrades, rejects writes with 'unavailable'; n1/n2 keep serving"):
        deadline = time.time() + 20
        s = ""
        ok = False
        while time.time() < deadline:
            s = status(n3)
            if "DEGRADED" in s:
                ok = True
                break
            time.sleep(1)
        assert ok, f"n3 never reported degraded: {s}"
        rc, out = kv(n3, "put /part/rejected x")
        assert rc != 0, f"partitioned n3 accepted a write: {out}"
        assert "unavailable" in out.lower(), f"rejection is not 'unavailable': {out}"
        rc, out = kv(n1, "put /part/alive during-partition")
        assert rc == 0, f"n1 stopped serving during the partition: {out}"

    with subtest("lease guard band: n3 self-released before TTL, takeover only after TTL"):
        assert wait_for_event(n3, "/tmp/lease-n3.log", "lost"), \
            "n3's holder never noticed the partition"
        rc, lease_log = n3.execute("cat /tmp/lease-n3.log")
        evs = [ln.split() for ln in lease_log.splitlines() if len(ln.split()) == 2 and ln.split()[1].isdigit()]
        renewed = [int(f[1]) for f in evs if f[0] == "renewed"]
        acquired = [int(f[1]) for f in evs if f[0] == "acquired"]
        lost = [int(f[1]) for f in evs if f[0] == "lost"]
        last_renew = renewed[-1] if renewed else acquired[0]
        # §4.3: the holder must stop acting (Done() closed) strictly
        # before the TTL elapses since its last renewal.
        assert lost[0] - last_renew < TTL_NS, \
            f"n3 self-released late: lost={lost[0]} last_renew={last_renew}"
        # ...while n1/n2 still see the lease as held until the TTL.
        rc, out = n2.execute("expanse ctl lease hold /part/lease --ttl 15s --timeout 10s --socket /run/expanse/agent.sock 2>&1")
        assert rc != 0, "n2 took the lease BEFORE the TTL elapsed"
        # After TTL + a 3s wall-clock margin, takeover succeeds.
        wait_ns = last_renew + TTL_NS + 3 * 10**9
        while time.time_ns() < wait_ns:
            time.sleep(1)
        n2.succeed(
            "nohup expanse ctl lease hold /part/lease --ttl 15s "
            "--timeout 300s --socket /run/expanse/agent.sock > /tmp/lease-n2.log 2>&1 &"
        )
        assert wait_for_event(n2, "/tmp/lease-n2.log", "acquired", 20), \
            "n2 could not acquire the lease after the TTL elapsed"

    with subtest("heal: n3 rejoins, catches up within 30 s, zero divergence"):
        n3.succeed("nft delete table ip exppart")
        deadline = time.time() + 30
        out = ""
        while time.time() < deadline:
            rc, out = kv(n3, "get /part/alive")
            if rc == 0 and out.strip() == "during-partition":
                break
            time.sleep(1)
        assert out.strip() == "during-partition", f"n3 did not catch up: {out!r}"
        s = status(n3)
        assert "DEGRADED" not in s, f"n3 still degraded after heal: {s}"
        rc, out = kv(n3, "put /part/after heal")
        assert rc == 0, f"n3 cannot write after heal: {out}"
        dumps = []
        for m in [n1, n2, n3]:
            rc, out = kv(m, "list /part/")
            assert rc == 0, out
            dumps.append("\n".join(sorted(out.strip().splitlines())))
        assert dumps[0] == dumps[1] == dumps[2], f"divergence after heal: {dumps!r}"
  '';
}
