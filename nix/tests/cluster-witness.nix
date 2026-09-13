# §6 cluster-witness: 2 workload nodes + 1 witness; killing a workload
# node still leaves a writable quorum (the witness votes, G3.6); the
# witness never serves the join endpoint and never receives placement;
# its memory footprint stays under 64 MB.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-cluster-witness";

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

    form("witness", role3="witness")

    with subtest("witness enrolled: role=witness, never serves :7446"):
        s = status(n1)
        row = [ln for ln in s.splitlines() if ln.split() and ln.split()[0] == "n3"]
        assert row and row[0].split()[1] == "witness", f"n3 is not a witness: {s}"
        rc, out = n3.execute("ss -tln | grep -c ':7446' || true")
        assert out.strip() == "0", f"witness serves the join endpoint: {out}"

    with subtest("seed canary"):
        rc, out = kv(n1, "put /wit/canary keepme")
        assert rc == 0, out

    with subtest("kill a workload node; witness + workload keep serving"):
        n1.succeed("systemctl stop expansed.service")
        # n1 must not be counted as leader by the survivors.
        deadline = time.time() + 20
        ok = False
        while time.time() < deadline:
            s = status(n2)
            if any(l in ("n2", "n3") for l in leaders(s)):
                ok = True
                break
            time.sleep(1)
        assert ok, f"survivors leaderless with the witness voting: {status(n2)}"
        rc, out = kv(n2, "put /wit/after failover-write")
        assert rc == 0, f"2/3 (witness voting) cannot serve writes: {out}"
        rc, out = kv(n3, "get /wit/after")
        assert out.strip() == "failover-write", f"witness cannot serve reads: {out}"

    with subtest("witness RSS < 64 MB (zero-capacity node)"):
        rc, out = n3.execute(
            "pid=$(systemctl show -p MainPID --value expansed); "
            "awk '/VmRSS/{print $2}' /proc/$pid/status; "
            "echo ROLLUP; cat /proc/$pid/smaps_rollup 2>/dev/null | head -8; "
            "echo MAPS; awk '{print $1, $2, $3, $6}' /proc/$pid/maps 2>/dev/null | sort | uniq -c | sort -rn | head -10"
        )
        rss_kb = int(out.splitlines()[0].strip() or 0)
        assert 0 < rss_kb < 65536, f"witness RSS {rss_kb} KiB is not < 64 MiB"

    with subtest("restart the workload node; full health"):
        n1.succeed("systemctl start expansed.service")
        n1.wait_for_unit("expansed.service")
        wait_quorum("3/2", 30)
        rc, out = kv(n1, "get /wit/after")
        assert out.strip() == "failover-write", f"n1 lost the failover write: {out}"
  '';
}
