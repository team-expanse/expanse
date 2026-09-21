"""vol-runtime testScript body (Phase 1 B4).

Drives the volume runtime (via test/volctl) on real LVM and DRBD across four
VMs: create from nothing, idempotence, drift, grow, and a rebuild that reuses a
recycled node-id for a different host.
"""

NAME = "vol-a1"
NODES = {"n1": "192.168.1.1", "n2": "192.168.1.2", "n3": "192.168.1.3", "n4": "192.168.1.4"}
ADDRS = ",".join(f"{h}={a}" for h, a in NODES.items())
DB = "/tmp/alloc.db"
MIB = 1024 * 1024
SIZE = 64 * MIB
GROWN = 96 * MIB
by_name = {"n1": n1, "n2": n2, "n3": n3, "n4": n4}


start_all()
for m in by_name.values():
    setup_node(m)

with subtest("nothing to a defined Secondary, unattended, on three nodes"):
    volctl(n1, f"alloc -db {DB} -name {NAME} -hosts n1,n2,n3")
    for h in ["n1", "n2", "n3"]:
        actions, forgot = reconcile(h, SIZE)
        assert actions == ["create-lv", "write-config", "create-md", "up"], f"{h}: {actions}"
        assert forgot == [], forgot
    for h, peers in [("n1", ["n2", "n3"]), ("n2", ["n1", "n3"]), ("n3", ["n1", "n2"])]:
        wait_until(by_name[h], connected_to(peers), "peers connected")
        assert f"{NAME} role:Secondary" in status(by_name[h]), f"{h} is not Secondary"
        assert device_bytes(by_name[h]) >= SIZE, f"{h}: device smaller than the {SIZE} requested"
    lvs = n1.succeed(f"lvs --noheadings --units b --nosuffix -o lv_name,lv_attr,lv_size vg0/{NAME}")
    assert " V" in lvs.replace(NAME, "", 1) or "Vwi" in lvs, f"backing LV is not thin: {lvs}"

with subtest("a second pass changes nothing"):
    for h in ["n1", "n2", "n3"]:
        assert reconcile(h, SIZE) == ([], []), f"{h} acted on a converged volume"

with subtest("drift is corrected by adjust, then settles"):
    n2.succeed(f"drbdsetup disconnect {NAME} 2 && drbdsetup del-peer {NAME} 2")
    assert not connected_to(["n3"])(status(n2)), "drift was not induced"
    assert reconcile("n2", SIZE) == (["adjust"], [])
    wait_until(n2, connected_to(["n1", "n3"]), "n3 reconnected after adjust")
    assert reconcile("n2", SIZE) == ([], [])

with subtest("initial data reaches every replica"):
    n1.succeed(f"drbdadm primary --force {NAME}")
    for h, peers in [("n1", ["n2", "n3"]), ("n2", ["n1", "n3"]), ("n3", ["n1", "n2"])]:
        wait_until(by_name[h], all_uptodate(peers), "UpToDate")
    n1.succeed("dd if=/dev/urandom of=/dev/drbd0 bs=1M count=32 oflag=direct 2>/dev/null")
    reference = head_sha(n1)
    n1.succeed(f"drbdadm secondary {NAME}")

with subtest("growing converges and keeps the data"):
    for _ in range(4):
        rounds = [reconcile(h, GROWN) for h in ["n1", "n2", "n3"]]
        print("grow round:", rounds)
        if all(not a for a, _ in rounds):
            break
    else:
        raise Exception("grow did not converge")
    for h in ["n1", "n2", "n3"]:
        assert device_bytes(by_name[h]) >= GROWN, f"{h}: device did not grow"
    n1.succeed(f"drbdadm primary {NAME}")
    assert head_sha(n1) == reference, "data changed across the grow"
    n1.succeed(f"drbdadm secondary {NAME}")

with subtest("rebuild on a new host reuses a recycled node-id"):
    # Spend every never-used id so the next assignment must recycle a forgotten one.
    for i in range(3, 32):
        volctl(n1, f"assign -db {DB} -name {NAME} -host ghost{i}")
        volctl(n1, f"retire -db {DB} -name {NAME} -host ghost{i}")
    assert volctl(n1, f"retire -db {DB} -name {NAME} -host n3").strip() == "2"
    n3.crash()

    for h in ["n1", "n2"]:
        actions, forgot = reconcile(h, GROWN)
        assert "adjust" in actions, f"{h} did not drop the dead peer: {actions}"
        assert 2 in forgot, f"{h} did not forget the dead node-id: {forgot}"
        for node_id in forgot:
            volctl(n1, f"ack -db {DB} -name {NAME} -id {node_id} -host {h}")

    fresh = int(volctl(n1, f"assign -db {DB} -name {NAME} -host n4").strip())
    assert fresh == 2, f"replacement got id {fresh}, expected the recycled id 2"

    for h in ["n4", "n1", "n2"]:
        actions, _ = reconcile(h, GROWN)
        print(f"rebuild {h}: {actions}")
    for h, peers in [("n1", ["n2", "n4"]), ("n2", ["n1", "n4"]), ("n4", ["n1", "n2"])]:
        wait_until(by_name[h], all_uptodate(peers), "n4 fully synced", timeout=240)
    assert "n3" not in status(n1), "the dead peer is still in the mesh"

    n4.succeed(f"drbdadm primary {NAME}")
    assert head_sha(n4) == reference, "the rebuilt replica does not hold the original data"
    n4.succeed(f"drbdadm secondary {NAME}")

with subtest("everything is converged again"):
    for h in ["n1", "n2", "n4"]:
        assert reconcile(h, GROWN)[0] == [], f"{h} still has work to do"
