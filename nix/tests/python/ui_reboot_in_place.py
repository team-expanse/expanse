"""ui-reboot-in-place: an automated rollout reboots a one-node cluster in place."""

try:
    with subtest("a one-node cluster runs a block with storage"):
        start_rebootable([n1, rel])
        form_single()
        rel.wait_for_open_port(8000)
        deploy(n1, "app", APP_MANIFEST, port=18090)
        wait_phase(n1, "app", ["RUNNING"], 240)
        boot = boot_id(n1)

    with subtest("an automated rollout reboots the lone node in place, into the release"):
        start_rollout()
        await_reboot(n1)
        assert boot_id(n1) != boot, "n1 kept its boot"
        runs_what_it_booted(n1)
        n1.wait_until_succeeds(f"expanse version | grep -q {TARGET}", timeout=120)
        wait_one_node_quorum(180)
        wait_for(lambda: kv_json(n1, "/upgrade/plan").get("state") == "done", "the rollout to finish", 600)
        job = kv_json(n1, "/upgrade/reboots/n1")
        assert job.get("state") == "done" and job.get("inPlace"), job
        rec = kv_json(n1, "/nodes/n1")
        assert not rec.get("cordoned") and not rec.get("reboot_drain"), rec

    with subtest("its block runs again"):
        wait_phase(n1, "app", ["RUNNING"], 300)
except Exception:
    dump_on_failure([n1])
    raise

print("UI-REBOOT-IN-PLACE PASSED: the one node rebooted in place into the release")
