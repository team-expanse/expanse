# §6 vol-durability (G6.3 — the release-blocker test). Runs the §4.8
# durability procedure: repeatedly write known-pattern records to the
# volume (fsync → ack), HARD-kill the volume-primary VM (qemu quit, not
# a clean shutdown), wait for failover, verify every acked record is
# present and byte-correct on the new primary, restore the killed node,
# wait for resync, and assert all 3 replicas' checksums agree.
#
# Any acked-write loss found here is a real bug in T04/T07/T11 — fix
# the protocol/recovery code; do not adjust the test.
#
# Iteration count: the flake check runs a compressed count (default 20)
# so `nix build` stays tractable; EXPANSE_DURABILITY_ITERS overrides at
# driver runtime (the driver runs outside the build sandbox when
# invoked via .driver, so CI's nightly job can do:
#   EXPANSE_DURABILITY_ITERS=500 \
#     $(nix build .#checks.x86_64-linux.vol-durability.driver --print-out-paths)/bin/nixos-test-driver
# for the full 500-iteration release run).
{ self }:
{ pkgs, lib, ... }:
let
  nodeCommon = idx: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
    ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    expanse.agent.exvolPool = "volumes";
    expanse.storage-test.enable = true;
    # Stable raft advertise across reboots: a crash-rejoin must not
    # re-register the per-boot SLIRP address (identical on every VM).
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    expanse.storage-test.poolSizeMB = 6144;
    boot.kernelModules = [ "nbd" ];
    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 12 * 1024;
    networking.firewall.interfaces.exp0.allowedTCPPorts = [ 9440 ];
    environment.systemPackages = with pkgs; [ zfs nbd python3 ];
  };
  # py_compile runs at BUILD time — a syntax error fails the check
  # build, not a 6-minute VM run.
  recScript = pkgs.runCommand "vol-durability-rec.py" { } ''
    ${pkgs.python3}/bin/python3 -m py_compile ${./python/vol_durability_rec.py}
    cp ${./python/vol_durability_rec.py} $out
  '';
in
{
  name = "expanse-vol-durability";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript =   ''
    ${builtins.readFile ./cluster-common.py}
    import os
    import random

    ITERS = int(os.environ.get("EXPANSE_DURABILITY_ITERS", "20"))
    rng = random.Random(0xD06AB1)  # deterministic: a failing seed reproduces
    print(f"vol-durability: {ITERS} iterations")

    # Record helpers live in an external python file (real file on disk
    # = lintable, py_compile-checked as a flake check — no more quoting
    # hell in inline snippets). Pushed to every VM at setup.
    REC = "/tmp/vol-durability-rec.py"
    for m in [n1, n2, n3]:
        m.copy_from_host("${recScript}", REC)

    form("voldur")

    acked = 0          # total records acked across the whole run
    next_seq = 0       # next block offset to write
    killed = None      # node currently crashed, if any

    def live_nodes():
        return [m for m in [n1, n2, n3] if m.name != (killed or "")]

    def vol_inspect(m):
        rc, out = m.execute(
            "expanse ctl volume inspect dur --socket /run/expanse/agent.sock 2>&1"
        )
        return out if rc == 0 else ""

    def machine_by_name(name):
        for mm in [n1, n2, n3]:
            if mm.name == name:
                return mm
        return None

    def primary_node():
        """Current volume primary from `volume inspect` (any live node)."""
        for m in live_nodes():
            out = vol_inspect(m)
            for ln in out.splitlines():
                if "primary:" in ln:
                    pid = ln.split()[-1]
                    mm = machine_by_name(pid)
                    if mm is not None:
                        return mm
        raise AssertionError("no primary in inspect output")

    def wait_primary_ready(timeout=240):
        """Failover (or steady state) done: a SURVIVING node is reported as
        the volume primary and (once alive) holds the device.

        Only surviving nodes are ever executed on: the test driver's
        execute() silently auto-restarts a crashed machine (connect() ->
        start()), which would defeat the hard-kill. The killed node is
        therefore excluded until its explicit restore.
        """
        deadline = time.time() + timeout
        while time.time() < deadline:
            for m in live_nodes():
                pid = ""
                for ln in vol_inspect(m).splitlines():
                    if "primary:" in ln:
                        pid = ln.split()[-1]
                if not pid or pid == (killed or ""):
                    continue
                p = machine_by_name(pid)
                if p not in live_nodes():
                    continue  # stale view naming the dead node
                rc, _ = p.execute("test -e /dev/exvol")
                if rc != 0:
                    continue
                holders = [mm for mm in live_nodes()
                           if mm.execute("ls /dev/exvol 2>/dev/null")[1].strip()]
                if len(holders) == 1 and holders[0] == p:
                    return p
            hs = wg_handshake_ages()
            if int(time.time()) % 60 < 3:
                print(f"HANDSHAKES {hs}")
            time.sleep(3)
        # NETEVIDENCE: the wait failed with every node's daemon alive but
        # no serving primary. VM runs have shown the exvol overlay
        # (10.42.x.1 wireguard) black-holing between crash-restored
        # peers while both daemons live — dump the network layer so the
        # product bug (if any) is diagnosable, not guessed at.
        for m in [n1, n2, n3]:
            print(f"NETEVIDENCE[{m.name}] wg:", m.execute(
                "wg show 2>&1 | head -40")[1])
            print(f"NETEVIDENCE[{m.name}] link:", m.execute(
                "ip -br a show exp0 2>&1; ip route 2>&1 | head -5")[1])
            print(f"NETEVIDENCE[{m.name}] phys:", m.execute(
                "ip -br a 2>&1 | head -6; ping -c1 -W1 192.168.1.2 2>&1 | tail -1; ping -c1 -W1 192.168.1.3 2>&1 | tail -1; ss -ulnp 2>/dev/null | grep 51820")[1])
            for peer in [n1, n2, n3]:
                if peer is m:
                    continue
                rc, out = m.execute(
                    f"ping -c1 -W1 10.42.{peer.name[1]}.1 2>&1 | tail -1")
                print(f"NETEVIDENCE[{m.name} -> {peer.name}] ping rc={rc}:", out)
            print(f"NETEVIDENCE[{m.name}] listen9440:", m.execute(
                "ss -ltn | grep 9440 2>&1")[1])
            print(f"NETEVIDENCE[{m.name}] nft9440:", m.execute(
                "nft list ruleset 2>/dev/null | grep -c 9440")[1])

        def wg_handshake_ages():
            """Compact per-minute handshake timeline: when did the mesh
            break? Print the age of the newest handshake per node while
            the wait polls, so a stale-handshake blackout can be lined
            up against crash/restore events.""
            out = []
            for m in [n1, n2, n3]:
                rc, txt = m.execute(
                    "wg show exp0 latest-handshakes 2>/dev/null")
                ages = []
                if rc == 0 and txt.strip():
                    import time as _t
                    now = _t.time()
                    for ln in txt.strip().splitlines():
                        parts = ln.split()
                        if len(parts) == 2:
                            try:
                                ages.append(int(now - int(parts[1])))
                            except ValueError:
                                pass
                out.append(f"{m.name}:[{','.join(str(a) for a in ages)}]")
            return " ".join(out)
        raise AssertionError(f"no ready primary within {timeout}s")
    def write_record(primary, seq):
        """Write record `seq` at block offset seq, fsync, then ack."""
        primary.succeed(f"python3 {REC} write {seq} /tmp/rec-{seq}")
        dev = "/dev/exvol/" + primary.succeed("ls -1 /dev/exvol").strip()
        primary.succeed(
            f"dd if=/tmp/rec-{seq} of={dev} bs=4096 seek={seq} conv=fsync,notrunc"
        )

    def check_records(primary, upto):
        """Every acked record must be present and byte-correct."""
        dev = "/dev/exvol/" + primary.succeed("ls -1 /dev/exvol").strip()
        for seq in range(upto):
            rc, _ = primary.execute(f"python3 {REC} check {seq} {dev}")
            assert rc == 0, f"ACKED WRITE LOST/CORRUPT: record {seq} on {primary.name} (release blocker)"

    def wait_resynced(timeout=300):
        """All 3 placements settled: no stale/resyncing roles left."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            out = vol_inspect(primary_node())
            rows = [ln.split() for ln in out.splitlines()
                    if len(ln.split()) >= 5 and ln.split()[1] in ("primary", "secondary", "stale", "resyncing")]
            if len(rows) == 3 and all(r[1] in ("primary", "secondary") for r in rows):
                return out
            time.sleep(3)
        raise AssertionError(f"resync did not settle within {timeout}s:\n{out}")

    def assert_3replica_checksums(timeout=420):
        """All 3 replicas' zvol heads must be checksum-equal (step 6).

        Polled: a restored node's daemon may still be catching up on the
        store (raft election) before it recreates its zvol and resyncs;
        placement ROWS can claim settled long before the data does."""
        deadline = time.time() + timeout
        sums = {}
        while time.time() < deadline:
            try:
                vol_id = primary_node().succeed("ls -1 /dev/exvol").strip()
            except AssertionError:
                time.sleep(5)
                continue
            sums, ok = {}, True
            for m in [n1, n2, n3]:
                rc, out = m.execute(
                    # iflag=direct: read the DISK, not the host page
                    # cache. The poll starts around the resync receive,
                    # so buffered reads can cache pre-receive content
                    # for the lifetime of the poll.
                    "dd iflag=direct if=/dev/zvol/volumes/volumes/"
                    + vol_id + " bs=4M count=4 2>/dev/null | sha256sum | cut -d' ' -f1"
                )
                if rc != 0 or not out.strip():
                    ok = False  # zvol not (re)created yet on this node
                    break
                sums[m.name] = out.strip()
            if ok and len(set(sums.values())) == 1:
                return
            time.sleep(5)
        if len(set(sums.values())) == 1 and len(sums) == 3:
            raise AssertionError(f"replicas did not converge within {timeout}s")
        # LOCALIZE: which 4KiB blocks of the 16MiB head differ?
        vol_id = primary_node().succeed("ls -1 /dev/exvol").strip()
        blocks = {}
        for m in [n1, n2, n3]:
            rc, out = m.execute(
                f"python3 {REC} blocks /dev/zvol/volumes/volumes/{vol_id} --direct"
            )
            blocks[m.name] = out.split() if rc == 0 else []
        diff = [i for i in range(len(blocks.get("n1", [])))
                if len(set(b[i] for b in blocks.values() if len(b) > i)) > 1]
        # EVIDENCE for post-mortem: per-node zvol identity + snapshots +
        # the first bytes of each differing block. A full send|receive
        # that exits 0 cannot differ from its source — this dump says
        # which side lied (stream content vs receive application).
        for m in [n1, n2, n3]:
            print(f"EVIDENCE[{m.name}] snaps:", m.execute(
                "zfs list -H -p -o name,used,written,creation -t snapshot 2>&1 | head -20"
            )[1])
            print(f"EVIDENCE[{m.name}] props:", m.execute(
                "zfs get -H -o property,value volblocksize,creation,used,compressratio "
                + "volumes/volumes/" + vol_id + " 2>&1"
            )[1])
            for i in diff[:4]:
                print(f"EVIDENCE[{m.name}] block {i}:", m.execute(
                    f"dd iflag=direct if=/dev/zvol/volumes/volumes/{vol_id} bs=4096 skip={i} "
                    "count=1 2>/dev/null | od -A d -t x1 | head -4"
                )[1])
        # Clone-read the SOURCE snapshots: does @resync-4 actually hold
        # the records? And does n1's RECEIVED @resync-4?
        pn = primary_node()
        print("EVIDENCE[pool] compression:", pn.execute(
            "zfs get -H -o property,value compression,recompress volumes 2>&1"
        )[1])
        for m, ds in [(pn, "volumes/volumes/" + vol_id), (n1, "volumes/volumes/" + vol_id)]:
            rc, sn = m.execute(
                "zfs list -H -o name -t snapshot 2>&1 | head -1"
            )
            sn = sn.strip()
            if not sn:
                continue
            m.execute(f"zfs clone {sn} volumes/evcheck 2>&1")
            rc, out = m.execute(
                "dd iflag=direct if=/dev/zvol/volumes/evcheck bs=4096 count=1 2>/dev/null "
                "| od -A d -t x1 | head -2"
            )
            print(f"EVIDENCE[{m.name}] clone({sn}) block0 rc={rc}:", out)
            m.execute("zfs destroy volumes/evcheck 2>&1")
        raise AssertionError(
            f"replica checksums DIVERGED after {timeout}s: {sums}; differing 4K blocks: {diff[:20]}")

    with subtest("volume created and primary attached"):
        n1.succeed("expanse ctl volume create dur --size 2Gi")
        for m in [n1, n2, n3]:
            m.wait_until_succeeds(
                "zfs list -H -o name -t volume | grep -q '^volumes/volumes/vol-'", timeout=90
            )
        wait_primary_ready()

    for it in range(ITERS):
        with subtest(f"iteration {it} (acked so far: {acked})"):
            # Steps 3-6 of the previous crash iteration have settled.
            primary = wait_primary_ready()
            check_records(primary, acked)

            # Step 1: write 1-3 records, fsync each, ack each.
            for _ in range(rng.randint(1, 3)):
                write_record(primary, next_seq)
                next_seq += 1
                acked += 1

            # Step 2: hard-kill the primary VM - qemu quit, no clean
            # shutdown, no unmount, no flush. Whatever was only in RAM is
            # gone; whatever was acked must survive.
            killed = primary.name
            primary.crash()
            print(f"iteration {it}: crashed primary {killed} after {acked} acked records")

            # Step 3: failover - some surviving node becomes primary.
            new_primary = wait_primary_ready(timeout=240)
            assert new_primary.name != killed, "primary failed over to the dead node"
            print(f"iteration {it}: failed over to {new_primary.name}")

            # Step 4+5: read EVERY acked record; all must be intact.
            check_records(new_primary, acked)

            # Step 6: restore the killed node, wait for resync, assert
            # all 3 replicas checksum-equal.
            dead = [m for m in [n1, n2, n3] if m.name == killed][0]
            dead.start()
            dead.wait_for_unit("multi-user.target", timeout=180)
            dead.wait_for_unit("expansed.service", timeout=120)
            wait_agent_ready(dead)
            wait_resynced(timeout=300)
            assert_3replica_checksums()
            killed = None
            print(f"iteration {it}: resynced; all 3 replicas agree ({acked} acked records intact)")

    with subtest("final: every acked record intact, all replicas equal"):
        primary = wait_primary_ready()
        check_records(primary, acked)
        assert_3replica_checksums()
        print(f"RELEASE-BLOCKER TEST PASSED: {acked} acked records survived "
              f"{ITERS} hard crashes with zero loss and 3-way checksum equality")

  '';
}
