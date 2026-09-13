# §6 cluster-node-loss: for each node i — kill i, the cluster keeps
# serving 2/3 (write+read), i restarts and catches up, full health
# returns. Zero data loss across all three losses (G3.5, G3.7).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-cluster-node-loss";

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

    form("node-loss")

    with subtest("lose each node in turn; 2/3 keeps serving; full health returns"):
        for i, victim in enumerate([n1, n2, n3]):
            survivors = [m for m in [n1, n2, n3] if m != victim]
            canary = f"/loss/n{i + 1}"
            rc, out = kv(survivors[0], f"put {canary} before-loss-{i}")
            assert rc == 0, f"pre-loss write failed: {out}"

            victim.succeed("systemctl stop expansed.service")

            # 2/3 keeps serving: a leader exists among the SURVIVORS —
            # not just any leader row, since a survivor's raft view can
            # briefly keep marking the dead victim as leader until its
            # own heartbeat timeout fires (QuorumHave counts configured
            # voters, so the number stays 3/2 either way).
            deadline = time.time() + 20
            ok = False
            while time.time() < deadline:
                s = status(survivors[0])
                if any(l in [m.name for m in survivors] for l in leaders(s)):
                    ok = True
                    break
                time.sleep(1)
            assert ok, f"{victim.name} down: survivors leaderless: {status(survivors[0])}"
            rc, out = kv(survivors[1], f"put {canary} after-loss-{i}")
            assert rc == 0, f"write failed with {victim.name} down: {out}"
            rc, out = kv(survivors[0], f"get {canary}")
            assert out.strip() == f"after-loss-{i}", f"read failed with {victim.name} down: {out}"

            # Restart: full health, victim caught up.
            victim.succeed("systemctl start expansed.service")
            victim.wait_for_unit("expansed.service")
            wait_quorum("3/2", 30)
            deadline = time.time() + 15
            out = ""
            while time.time() < deadline:
                rc, out = kv(victim, f"get {canary}")
                if rc == 0 and out.strip() == f"after-loss-{i}":
                    break
                time.sleep(1)
            assert out.strip() == f"after-loss-{i}", f"{victim.name} did not catch up: {out}"
            print(f"node-loss round {i + 1} (lost {victim.name}): OK")

    with subtest("zero data loss across all three losses (every node agrees)"):
        for n in range(3):
            canary = f"/loss/n{n + 1}"
            want = f"after-loss-{n}"
            for m in [n1, n2, n3]:
                rc, out = kv(m, f"get {canary}")
                assert out.strip() == want, f"{m.name} disagrees on {canary}: {out!r}"
  '';
}
