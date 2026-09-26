"""cluster-single-node (Phase 12 C1): one node is a working cluster.

A cluster formed on n1 alone serves a default volume (placed 1 of target 3, shown as having no
redundancy), a block with storage and no replication, and a VIP with one candidate; an explicit
--replication 3 waits with its reason shown. Everything survives a reboot.

Runs after cluster-common.py, block-common.py, vol_cluster.py and single_node_common.py;
n9 is the external client.
"""

MARKER = "single-node-marker"
APP_MOUNT = "/mnt/app-data"
APP_VOL = "blk-default-app-data"  # storage.BlockVolumeName(ns, block, storageName)

APP_MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: app
  namespace: default
spec:
  type: util/echo
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 64Mi
  storage:
    - name: data
      size: 32Mi
      mountPath: {APP_MOUNT}
  config:
    port: 18090
    body: "single\\n"
"""

WEB_MANIFEST = (
    "apiVersion: expanse.io/v1\nkind: Block\n"
    "metadata:\n  name: web\n  namespace: default\n"
    "spec:\n  type: web/nginx\n  replicas: 1\n"
    "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
    "  config:\n    port: 8080\n    serverName: web\n"
    "  network:\n    ports:\n"
    "      - name: http\n        port: 80\n        target_port: 8080\n"
    "        protocol: tcp\n        expose: EXPOSE_VIP\n"
    "    health_check:\n      readiness:\n        type: PROBE_TCP\n"
    "        port: 8080\n        period_seconds: 2\n"
)


def volume_line(name):
    for line in n1.execute("expanse ctl volume list 2>&1")[1].splitlines():
        cols = line.split()
        if len(cols) > 1 and cols[1] == name:
            return line
    return ""


def is_lone_and_flagged(name):
    row = volume_row(n1, name)
    line = volume_line(name)
    return row is not None and row["state"] == "underreplicated" and row["nodes"] == ["n1"] \
        and " 1/3 " in line and "no redundancy" in line


def block_vip():
    """The web block's address; the pool's other one is the web UI's own VIP."""
    out = n1.execute("expanse ctl kv --socket /run/expanse/agent.sock get /network/vipPool/external/default/web 2>&1")[1]
    found = re.search(r'"addr":\s*"([0-9.]+)/', out)
    return found.group(1) if found else ""


def vip_serves():
    vip = block_vip()
    out = n9.execute(f"curl -s -o /dev/null -w '%{{http_code}}' --connect-timeout 3 http://{vip}/ || true")[1]
    return vip != "" and out.strip() == "200"


def app_mount():
    row = volume_row(n1, APP_VOL)
    return f"/var/lib/expanse/volumes/{row['id']}/mnt" if row else None


def dump_on_failure():
    print(status(n1))
    print(n1.execute("expanse ctl volume list 2>&1")[1])
    print(n1.execute("journalctl -u expansed.service --no-pager | tail -60")[1])


try:
    with subtest("a cluster forms on one node"):
        n9.start()
        form_single()

    with subtest("a default volume is placed on the one node and says it has no redundancy"):
        n1.succeed("expanse ctl volume create solo --size 64Mi")
        wait_for(lambda: is_lone_and_flagged("solo"), "solo to be placed 1/3 with no redundancy", 120)
        out = n1.succeed("expanse ctl volume inspect solo")
        assert "replicas: 1 of 3 (no redundancy)" in out, out

    with subtest("an explicit replication 3 waits, with its reason shown"):
        n1.succeed("expanse ctl volume create strict --size 64Mi --replication 3")
        wait_for(lambda: "pending: needs 3 nodes, 1 eligible" in volume_line("strict"),
                 "the pending reason for strict", 60)

    with subtest("a block with storage and no replication runs and writes to its volume"):
        deploy(n1, "app", APP_MANIFEST)
        wait_phase(n1, "app", ["RUNNING"], 180)
        wait_for(lambda: is_lone_and_flagged(APP_VOL), "the block's volume to be placed 1/3", 60)
        n1.wait_until_succeeds(f"mountpoint -q {app_mount()}", timeout=60)
        n1.succeed(f"echo -n {MARKER} > {app_mount()}/marker && sync")

    with subtest("a VIP with one candidate is held and serves"):
        deploy(n1, "web", WEB_MANIFEST)
        wait_phase(n1, "web", ["RUNNING"], 120)
        wait_for(lambda: block_vip() != "", "the web block's VIP allocation", 60)
        n1.wait_until_succeeds(f"ip -4 -o addr show eth1 | grep -qF {block_vip()}", timeout=60)
        wait_for(vip_serves, "the client to reach the VIP", 60)

    with subtest("everything is back after a reboot"):
        n1.shutdown()
        n1.start()
        n1.wait_for_unit("expansed.service")
        wait_agent_ready(n1)
        wait_one_node_quorum(180)
        wait_phase(n1, "app", ["RUNNING"], 240)
        n1.wait_until_succeeds(f"mountpoint -q {app_mount()}", timeout=120)
        got = n1.succeed(f"cat {app_mount()}/marker").strip()
        assert got == MARKER, f"marker after reboot = {got!r}"
        wait_for(lambda: is_lone_and_flagged("solo"), "solo to still be 1/3", 60)
        assert "pending: needs 3 nodes, 1 eligible" in volume_line("strict"), volume_line("strict")
        wait_phase(n1, "web", ["RUNNING"], 120)
        wait_for(vip_serves, "the client to reach the VIP after the reboot", 90)
except Exception:
    dump_on_failure()
    raise

print("SINGLE-NODE PASSED: default volume 1/3, block storage, VIP and reboot on one node")
