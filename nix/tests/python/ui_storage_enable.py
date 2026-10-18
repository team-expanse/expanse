"""ui-storage-enable (Phase 14 E): storage turned on from a node's page, with no Nix edit or reboot.

n1 has the installer's data VG and nothing built on it. The node reports storage off, the deploy
form for a type with storage says so, and pressing Enable storage makes the node build its thin
pool and start its volume stack, so a volume created afterwards comes up healthy.
Runs after cluster-common.py, block-common.py, vol_cluster.py, single_node_common.py and
ui_forms_common.py.
"""

PASSWORD = "ui-storage-enable-test-password"


def report():
    out = kv(n1, "get /storage-nodes/n1")[1]
    try:
        return json.loads(out)
    except ValueError:
        return {}


def page(path):
    return n1.succeed(f"{CURL} https://n1:8443{path}")


n1.wait_for_unit("data-vg.service")
form_single("storage")
n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")
csrf = ui_login()

with subtest("the node boots with DRBD 9 and reports storage off"):
    n1.succeed("head -1 /proc/drbd | grep -q 'version: 9\\.'")
    wait_for(lambda: report().get("state") == "off" and report().get("vgFree", 0) > 0, "an off report with a free VG", 60)
    body = page("/nodes/n1")
    assert "Enable storage" in body, "the node page does not offer Enable storage"
    assert "No node has storage turned on" in page("/deploy/vm/instance"), "the deploy form does not warn"

with subtest("Enable storage builds the pool and starts volumes"):
    status, loc, body = post("/nodes/n1/storage", {"percent": "90"})
    assert status == "303", f"enable: {status}\n{body}"
    assert loc.startswith("/nodes/n1"), loc
    wait_for(lambda: report().get("state") == "ready", "storage ready", 90)
    lv = n1.succeed("lvs --noheadings -o lv_attr expanse/pool").strip()
    assert lv.startswith("t"), f"expanse/pool is not a thin pool: {lv}"
    assert "No node has storage turned on" not in page("/deploy/vm/instance"), "the warning stays"
    assert "Enable storage</button>" not in page("/nodes/n1"), "the button stays"

with subtest("a volume created afterwards comes up healthy"):
    n1.succeed("expanse ctl volume create v1 --size 64Mi --replication 1")
    wait_for(lambda: (volume_row(n1, "v1") or {}).get("state") == "healthy", "v1 healthy", 120)
    n1.succeed("lvs --noheadings -o pool_lv expanse | grep -q pool")

print("UI-STORAGE-ENABLE DONE")
