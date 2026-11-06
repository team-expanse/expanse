"""vm-console: a VM's serial console, from the CLI and the web UI, on its node and from another.

`expanse ctl vm console` and the console page's WebSocket both attach to the guest's ttyS0 on
whichever node runs it. The netboot guest logs in on its serial console, so each check types a
command there and waits for its output. The guest's console output also still reaches the journal.

Runs after cluster-common.py, block-common.py and vol_cluster.py, with KERNEL, INITRD, CMDLINE
and GUEST_IP (vm-instance.nix) spliced in ahead of this file.
"""

NAME = "vm1"
UNIT = f"expanse-block-root@default-{NAME}-0.service"
SW = "/run/current-system/sw/bin"
UI_PASSWORD = "console-password"
UI_CAFILE = "/persist/expanse/ca/ui-ca.pem"
JAR = "/root/ui-cookies.txt"
WS = "/etc/expanse-test/console_ws.py"

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {NAME}
  namespace: default
spec:
  type: vm/instance
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 1000m
      memory: 2Gi
  storage:
    - name: disk
      size: 16Mi
      replication: 3
      mountPath: /mnt/{NAME}-disk
      filesystem: none
  placement:
    requiredCapabilities: [kvm]
  config:
    bootKernel: "{KERNEL}"
    bootInitrd: "{INITRD}"
    bootCmdline: "{CMDLINE}"
"""

ALL = {"n1": n1, "n2": n2, "n3": n3}


def host_of():
    nodes = placement_nodes(get_json(n1, NAME) or {})
    assert len(nodes) == 1, f"{NAME} placed on {nodes}"
    name = next(iter(nodes))
    return name, ALL[name]


def cli_console(m, tag):
    """Type `echo <tag>-$((6*7))` at the console from m through the CLI; returns what it printed."""
    out = m.succeed(
        f"(sleep 1; printf '\\r'; sleep 1; printf 'echo {tag}-$((6*7))\\r'; sleep 4) "
        f"| timeout 60 expanse ctl vm console {NAME}"
    )
    assert f"{tag}-42" in out, f"the console from {m.name} never ran the command:\n{out[-2000:]}"
    return out


def ui_login(m):
    m.succeed(f"rm -f {JAR}")
    code = m.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' -c {JAR} --cacert {UI_CAFILE} "
        f"-d 'username=admin&password={UI_PASSWORD}' https://{m.name}:8443/login"
    ).strip()
    assert code == "303", f"login on {m.name} returned {code}, want 303"


def ws_console(m, tag):
    """Type the same command through m's web UI console socket."""
    path = f"/blocks/default/{NAME}/console/socket?replica=0"
    rc, out = m.execute(f"python3 {WS} {m.name} '{path}' {JAR} {UI_CAFILE} '\\recho {tag}-$((6*7))\\r' {tag}-42 30")
    assert rc == 0, f"the web console on {m.name} never ran the command:\n{out[-2000:]}"


form("vmcon")
for m in ALL.values():
    wait_agent_ready(m)
n1.succeed(f"expanse ctl admin reset-password --password '{UI_PASSWORD}'")

with subtest("deploy a VM and wait for its guest"):
    deploy(n1, NAME, MANIFEST)
    wait_phase(n1, NAME, ["RUNNING"], 300)
    host_name, host = host_of()
    other = next(m for n, m in ALL.items() if n != host_name)
    host.wait_until_succeeds(f"expanse ctl vm exec {NAME} -- {SW}/true", timeout=180)
    print(f"{NAME} runs on {host_name}; {other.name} reaches it from another node")

with subtest("the CLI console on the VM's node runs a command in the guest"):
    cli_console(host, "LOCAL")

with subtest("the console still writes the guest's output to the journal"):
    # -a: lines carrying the guest's terminal escapes are otherwise shown as blobs.
    host.succeed(f"journalctl -a -u {UNIT} --no-pager | grep -q 'Booting initrd'")

with subtest("the CLI console works from another node"):
    cli_console(other, "REMOTE")

with subtest("a missing block is refused with a reason"):
    rc, out = other.execute("expanse ctl vm console nosuch </dev/null 2>&1")
    assert rc != 0 and "nosuch" in out, f"console to a missing block: rc={rc} {out}"

with subtest("the web UI's console socket reaches the guest, from its node and another"):
    for m in (host, other):
        ui_login(m)
        ws_console(m, f"WEB{m.name.upper()}")

with subtest("a cross-origin console socket is refused"):
    ui_login(other)
    code = other.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' -b {JAR} --cacert {UI_CAFILE} "
        "-H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' "
        "-H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' -H 'Origin: https://evil.example' "
        f"'https://{other.name}:8443/blocks/default/{NAME}/console/socket?replica=0'"
    ).strip()
    assert code == "403", f"cross-origin upgrade answered {code}, want 403"
    print("VM-CONSOLE DONE")
