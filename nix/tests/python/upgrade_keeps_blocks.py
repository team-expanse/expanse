"""node-upgrade-keeps-blocks (Phase 15 A3): an agent restart and a switch to a new expanse build
leave a running block, and the volume it holds open, alone.

Runs after cluster-common.py, block-common.py (json), vol_cluster.py and single_node_common.py.
"""

APP_MOUNT = "/mnt/app-data"
APP_VOL = "blk-default-app-data"
APP_UNIT = "expanse-block@default-app-0.service"
APP_SPEC = "/run/expanse/block-replica/default-app-0.json"
# Shell words: a space after the --config JSON's opening brace, which no agent build writes; and a match for it.
RESPACE = """'s/"--config","{/"--config","{ /'"""
SPACED = """'"--config","{ '"""
NEW_VERSION = "99.0.0-upgraded"

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
    body: "kept\\n"
"""


def app_mount():
    row = volume_row(n1, APP_VOL)
    return f"/var/lib/expanse/volumes/{row['id']}/mnt" if row else None


def roles():
    out = n1.succeed("drbdsetup status --json")
    return [r["role"] for r in json.loads(out)]


def main_pid():
    return n1.succeed(f"systemctl show -P MainPID {APP_UNIT}").strip()


def lines_written():
    return int(n1.succeed(f"wc -l < {app_mount()}/writer.log").strip())


def ticks():
    out = n1.succeed("journalctl -u expansed.service -o cat | grep -c 'msg=\"tick complete\"' || true")
    return int(out.strip() or 0)


def assert_untouched(pid, what):
    """The block's process, the writer and the volume's primary role all survived `what`."""
    assert main_pid() == pid, f"the block restarted across {what}: {pid} -> {main_pid()}"
    n1.succeed("systemctl is-active writer.service")
    assert roles() == ["Primary"], f"volume roles after {what}: {roles()}"
    before = lines_written()
    wait_for(lambda: lines_written() > before + 3, f"the writer to keep writing after {what}", 30)
    wait_phase(n1, "app", ["RUNNING"], 60)
    echo_responds(n1, 18090, body="kept")


def dump_on_failure():
    print(status(n1))
    print(n1.execute("expanse ctl volume list 2>&1")[1])
    print(n1.execute("drbdsetup status --verbose 2>&1")[1])
    print(n1.execute(f"systemctl status {APP_UNIT} writer.service --no-pager 2>&1")[1])
    print(n1.execute("journalctl -u expansed.service --no-pager | tail -80")[1])


try:
    with subtest("a block runs with a writer holding its volume open"):
        form_single()
        deploy(n1, "app", APP_MANIFEST, port=18090)
        wait_phase(n1, "app", ["RUNNING"], 180)
        n1.wait_until_succeeds(f"mountpoint -q {app_mount()}", timeout=60)
        # Absolute paths: /run/current-system/sw/bin changes under the writer during the switch.
        sleep = n1.succeed("readlink -f $(command -v sleep)").strip()
        bash = n1.succeed("readlink -f $(command -v bash)").strip()
        n1.succeed(
            f"systemd-run --unit=writer {bash} -c 'exec 3>>{app_mount()}/writer.log; "
            f"i=0; while true; do echo $i >&3; i=$((i+1)); {sleep} 0.2; done'")
        pid = main_pid()
        assert pid != "0", "the block has no main process"

    with subtest("restarting the agent leaves the block and its volume alone"):
        start = time.monotonic()
        n1.succeed("systemctl restart expansed.service")
        took = time.monotonic() - start
        assert took < 20, f"the agent took {took:.0f}s to restart; a busy volume must not be retried"
        wait_agent_ready(n1)
        assert_untouched(pid, "an agent restart")
        n1.succeed("journalctl -u expansed.service | grep -q 'left primary for the next agent'")

    with subtest("a config encoded with other spacing, as another agent build may write it, is no drift"):
        n1.fail(f"grep -qF {SPACED} {APP_SPEC}")
        n1.succeed(f"sed -i {RESPACE} {APP_SPEC}")
        n1.succeed(f"grep -qF {SPACED} {APP_SPEC}")
        before = ticks()
        wait_for(lambda: ticks() >= before + 3, "three reconcile ticks over the respaced spec", 60)
        assert_untouched(pid, "a respaced config")

    with subtest("switching to a new expanse build leaves the block and its volume alone"):
        n1.succeed("/run/current-system/specialisation/upgraded/bin/switch-to-configuration test")
        wait_agent_ready(n1)
        assert NEW_VERSION in n1.succeed("expanse version"), n1.succeed("expanse version")
        assert_untouched(pid, "the switch")
        n1.fail("journalctl -u expansed.service | grep -q 'leading interrupted'")
except Exception:
    dump_on_failure()
    raise

print("UPGRADE-KEEPS-BLOCKS PASSED: agent restart and switch left the block and its volume alone")
