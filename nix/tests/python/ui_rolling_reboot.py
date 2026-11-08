"""ui-rolling-reboot: an automated rollout drains, reboots and restores each node of three.

`app` (a singleton with a replicated volume) moves off each node before it reboots; `spread`
(one replica per node) has nowhere to go, so each node holds its replica through the reboot.
"""

NODES3 = [n1, n2, n3]

try:
    with subtest("a 3-node cluster runs a movable block and one on every node"):
        start_rebootable(NODES3 + [rel])
        form()
        rel.wait_for_open_port(8000)
        deploy(n1, "app", APP_MANIFEST, port=18090)
        wait_phase(n1, "app", ["RUNNING"], 240)
        deploy(n1, "spread", echo_yaml("spread", replicas=3, port=18081), port=18081)
        wait_phase(n1, "spread", ["RUNNING"], 240)
        boots = {m.name: boot_id(m) for m in NODES3}
        assert leader_of(status(n1)) == "n1", "n1 must lead, so the rollout ends on it"

    with subtest("Upgrade cluster with reboots on starts an automated rollout"):
        start_rollout()

    with subtest("each node upgrades and reboots into the release in turn, the leader last"):
        for m in [n2, n3, n1]:
            await_reboot(m)
            assert boot_id(m) != boots[m.name], f"{m.name} kept its boot"
            runs_what_it_booted(m)
            m.wait_until_succeeds(f"expanse version | grep -q {TARGET}", timeout=120)

    with subtest("the rollout finishes with every node back in service"):
        wait_for(lambda: kv_json(n2, "/upgrade/plan").get("state") == "done", "the rollout to finish", 900)
        jobs = {m.name: kv_json(n2, f"/upgrade/reboots/{m.name}") for m in NODES3}
        for name, job in jobs.items():
            assert job.get("state") == "done" and not job.get("inPlace"), f"{name}: {job}"
            held = job.get("held") or {}
            assert any(r.startswith("default/spread") for r in held), f"{name} held no spread replica: {job}"
            assert not any(r.startswith("default/app") for r in held), f"{name} held app, which could move: {job}"
            rec = kv_json(n2, f"/nodes/{name}")
            assert not rec.get("cordoned") and not rec.get("draining") and not rec.get("reboot_drain"), rec
            assert not kv_json(n2, f"/upgrade/nodes/{name}").get("rebootNeeded"), f"{name} still waits for a reboot"
        order = sorted(jobs, key=lambda n: jobs[n]["started"])
        assert order == ["n2", "n3", "n1"], f"reboot order {order}"
        for a, b in zip(order, order[1:]):  # RFC 3339 in UTC: comparable as strings
            assert jobs[a]["updated"] <= jobs[b]["started"], f"{a} and {b} rebooted at once: {jobs}"

    with subtest("the blocks run on the upgraded cluster"):
        wait_quorum("3/2", 180)
        wait_phase(n2, "app", ["RUNNING"], 240)
        wait_phase(n2, "spread", ["RUNNING"], 240)
except Exception:
    dump_on_failure(NODES3)
    raise

print("UI-ROLLING-REBOOT PASSED: each node drained, rebooted into the release and went back in service")
