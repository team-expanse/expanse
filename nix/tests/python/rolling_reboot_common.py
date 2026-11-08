"""Rolling-reboot helpers: nodes that reboot themselves through a real boot loader.

`rel` serves a signed release whose node-system.nix is each node's prebuilt `upgraded`
specialisation, which has a new initrd, so its upgrade needs a reboot.
Runs after cluster-common.py, block-common.py, vol_cluster.py and ui_forms_common.py.
"""

PASSWORD = "rolling-reboot-password"
TARGET = "99.0.0"
SOCK = "--socket /run/expanse/agent.sock"

APP_MANIFEST = """apiVersion: expanse.io/v1
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
      mountPath: /mnt/app-data
  config:
    port: 18090
    body: "rebooted\\n"
"""


def kv_json(m, key):
    rc, out = m.execute(f"expanse ctl kv {SOCK} get {key} 2>/dev/null")
    found = re.search(r"\{.*\}", out, re.S)
    return json.loads(found.group(0)) if rc == 0 and found else {}


def boot_id(m):
    return m.succeed("cat /proc/sys/kernel/random/boot_id").strip()


def start_rebootable(machines):
    """Starts each machine so that a reboot from inside it restarts it, not ends it."""
    for m in machines:
        m.start(allow_reboot=True)


def await_reboot(m, timeout=1500):
    """Waits for m to reboot itself, then reconnects to its new boot."""
    m.wait_for_console_text("reboot: Restarting system", timeout=timeout)
    m.connected = False
    m.wait_for_unit("expansed.service")
    wait_agent_ready(m)


def runs_what_it_booted(m):
    m.succeed('[ "$(readlink -f /run/booted-system)" = "$(readlink -f /run/current-system)" ]')


def start_rollout():
    """Offers the release and starts a rollout with "Reboot nodes that need it" on."""
    global csrf
    n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")
    csrf = ui_login()
    latest = json.dumps({"version": TARGET, "releases": [TARGET], "disabled": True})
    n1.succeed(f"expanse ctl kv {SOCK} put /upgrade/latest {shlex.quote(latest)}")
    code, _, body = post("/upgrade/start", {"version": TARGET, "reboot": "on"})
    assert code == "303", f"start: {code}\n{body}"
    assert kv_json(n1, "/upgrade/plan").get("reboot"), "the rollout does not reboot nodes"


def dump_on_failure(machines):
    for m in machines:
        if not m.connected:
            continue
        print(m.name, kv_json(m, "/upgrade/plan"))
        print(m.execute(f"expanse ctl kv {SOCK} list /upgrade/reboots/ 2>&1 | head -40")[1])
        print(m.execute("journalctl -u expanse-upgrade --no-pager | tail -30")[1])
        print(m.execute("journalctl -u expansed --no-pager | grep -i -E 'reboot|rollout|drain' | tail -30")[1])
