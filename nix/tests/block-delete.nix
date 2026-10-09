# §8 block-delete: deploy util/echo replicas=3, all Running, then
# delete; assert: units gone, cgroups gone, store keys gone, resources
# freed, no journald identifier remaining, `expanse ctl block list`
# empty (G4.13). Exercises the T22 reconciler-Deleter path (the delete
# removes the desired /node/<id>/resources/... keys; the reconciler
# must stop and remove the units).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-block-delete";

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
      environment.systemPackages = with pkgs; [ openssl curl jq ];
      virtualisation.memorySize = 2048;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
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
      environment.systemPackages = with pkgs; [ openssl curl ];
      virtualisation.memorySize = 2048;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
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
      environment.systemPackages = with pkgs; [ openssl curl ];
      virtualisation.memorySize = 2048;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}

    form("test")

    with subtest("deploy util/echo replicas=3, all running"):
        deploy(n1, "web", echo_yaml("web", 3, 18080, "delete-test\n"))
        b = wait_phase(n1, "web", ["RUNNING"], 30)
        assert len(placement_nodes(b)) == 3, f"want 3 nodes: {b.get('status')}"

    # Baseline: units active on all three nodes; remember the journald
    # watermark so the post-delete check can assert NO new lines.
    # Anti-affinity spreads the replicas: exactly ONE unit per node
    # (indices 0/1/2 across n1/n2/n3).
    ok = False
    deadline = time.time() + 20
    while time.time() < deadline:
        counts = [len((m.execute("systemctl list-units 'expanse-block@*' --no-legend || true")[1]).strip().splitlines())
                  for m in [n1, n2, n3]]
        if counts == [1, 1, 1]:
            ok = True
            break
        time.sleep(2)
    assert ok, f"expected exactly one block unit per node: {counts}"
    # Baseline: remember the journald watermark so the post-delete
    # check can assert NO new lines from any replica identifier.
    for m in [n1, n2, n3]:
        m.execute(
            "journalctl -t expanse-block-default-web-0"
            " -t expanse-block-default-web-1"
            " -t expanse-block-default-web-2"
            " --since '-1s' -q --no-pager | wc -l > /tmp/jw")

    with subtest("delete"):
        n1.succeed(f"expanse ctl block delete {SOCK} -n default web")

    def units_gone(m):
        rc, out = m.execute("systemctl list-units 'expanse-block@*' --no-legend || true")
        return out.strip() == ""

    with subtest("units and cgroups gone, resources freed within 45 s"):
        deadline = time.time() + 45
        while time.time() < deadline:
            if all(units_gone(m) for m in [n1, n2, n3]):
                break
            time.sleep(2)
        assert all(units_gone(m) for m in [n1, n2, n3]), "expanse-block@ units still present"
        # Cgroups: the slice has no per-replica service dirs left.
        for m in [n1, n2, n3]:
            rc, out = m.execute(
                "systemd-cgls /sys/fs/cgroup/expanse-blocks.slice --no-pager 2>/dev/null || true")
            assert "expanse-block@" not in out, f"cgroup dirs remain on {m.name}: {out}"
            # Resources freed: no processes left under the slice.
            rc, out = m.execute(
                "systemctl show expanse-blocks.slice -p TasksCurrent --value")
            assert out.strip() in ("", "0"), f"slice still runs {out!r} tasks on {m.name}"

    with subtest("store keys gone"):
        deadline = time.time() + 20
        while time.time() < deadline:
            rc, out = n1.execute(
                f"expanse ctl kv {SOCK} list /node 2>/dev/null | grep -c block-replica || true")
            if out.strip() in ("", "0"):
                break
            time.sleep(2)
        rc, out = n1.execute(
            f"expanse ctl kv {SOCK} list /node 2>/dev/null | grep -c block-replica || true")
        assert out.strip() in ("", "0"), f"block-replica desired keys remain: {out}"
        b = get_json(n1, "web")
        assert b is None, f"block still in store: {b}"

    with subtest("no journald identifier remaining"):
        # No NEW log lines after the delete (history is allowed to remain —
        # §5.5 is about live units, not log retention). Counting twice, not
        # --since, so a stop line logged just before the check is not "new".
        count = ("journalctl -t expanse-block-default-web-0 -t expanse-block-default-web-1"
                 " -t expanse-block-default-web-2 -q --no-pager | wc -l")
        before = {m.name: m.succeed(count).strip() for m in [n1, n2, n3]}
        time.sleep(5)
        for m in [n1, n2, n3]:
            after = m.succeed(count).strip()
            assert after == before[m.name], f"new journald lines after delete on {m.name}: {before[m.name]} -> {after}"

    with subtest("block list empty"):
        rc, out = n1.execute(f"expanse ctl block list {SOCK} -o json")
        assert out.strip() in ("", "null", "[]", "{}"), f"block list not empty: {out}"

    with subtest("cluster still healthy after teardown"):
        wait_quorum("3/2", 30)
  '';
}
