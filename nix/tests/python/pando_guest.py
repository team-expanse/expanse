"""Pando in a vm/instance guest (docs/PANDO.md), across a crash of the node running it.

Pre-creates the block's volume, writes the Pando guest image onto it, and deploys a
vm/instance that BIOS-boots from it. A group created through Pando's API before the
serving node is crashed must still be there once the guest has cold-booted on a survivor.

Runs after cluster-common.py, block-common.py and vol_cluster.py. Expects IMAGE,
GUEST_IP and ADMIN_PW (pando-guest.nix) spliced in ahead of this file; json comes from
block-common.py.
"""

NAME = "pando"
VOL_NAME = f"blk-default-{NAME}-disk"  # storage.BlockVolumeName: the block adopts this volume
VOL_SIZE = "10Gi"
UNIT = f"expanse-block-root@default-{NAME}-0.service"
CONSOLE = f"http://{GUEST_IP}:8080"
ALL = {"n1": n1, "n2": n2, "n3": n3}
GROUP = "survives-failover"

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
      memory: 3Gi
  storage:
    - name: disk
      size: {VOL_SIZE}
      replication: 3
      mountPath: /mnt/{NAME}-disk
      filesystem: none
  placement:
    requiredCapabilities: [kvm]
"""


def write_image_paced(m, dev, chunk_mib=64, pause_s=0.3):
    """The image onto dev in chunks with pauses; one long dd saturates the test link and trips raft."""
    size = int(m.succeed(f"stat -c %s {IMAGE}").strip())
    chunks = (size + chunk_mib * 2**20 - 1) // (chunk_mib * 2**20)
    for i in range(chunks):
        m.succeed(f"dd if={IMAGE} of={dev} bs=1M skip={i * chunk_mib} seek={i * chunk_mib} "
                  f"count={chunk_mib} conv=sparse,notrunc oflag=direct status=none")
        time.sleep(pause_s)
    m.succeed(f"sync {dev}")


def api(m, method, path, body=None):
    """curl against Pando's API from m with the session cookie in /tmp/pando.jar; returns (status, text)."""
    data = f"-H 'Content-Type: application/json' -d '{json.dumps(body)}'" if body is not None else ""
    out = m.succeed(f"curl -s -m 10 -b /tmp/pando.jar -c /tmp/pando.jar -X {method} {data} "
                    f"-w '\\n%{{http_code}}' {CONSOLE}{path}")
    text, _, code = out.rpartition("\n")
    return int(code), text


def sign_in(m):
    m.succeed("rm -f /tmp/pando.jar")
    code, text = api(m, "POST", "/api/v1/sessions", {"username": "admin", "password": ADMIN_PW})
    assert code in (200, 201, 204), f"sign-in failed: {code} {text}"


def console_up(m):
    return m.execute(f"curl -sf -m 5 -o /dev/null {CONSOLE}/")[0] == 0


def guest_console(m):
    return m.execute(f"journalctl -u {UNIT} --no-pager -o cat 2>&1 | tail -40")[1]


form("pando")
for m in ALL.values():
    wait_agent_ready(m)

with subtest("pre-create the block's volume and write the Pando image onto it"):
    n1.succeed(f"expanse ctl volume create {VOL_NAME} --size {VOL_SIZE} --replication 3")
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    wait_for(lambda: len(primaries(res)) == 1, "one primary")
    seeder = primaries(res)[0]
    t = time.time()
    write_image_paced(seeder, device_of(seeder))
    print(f"image written on {seeder.name} in {time.time() - t:.0f}s")
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "replicas UpToDate after the write", 300)

with subtest("a vm/instance block adopts the volume and the guest boots Pando"):
    deploy(n1, NAME, MANIFEST)
    b = wait_phase(n1, NAME, ["RUNNING"], 900)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"placed on {nodes}: {b.get('status')}"
    holder = next(iter(nodes))
    other = next(m for n, m in ALL.items() if n != holder)  # a host cannot reach its own macvtap guest (A34)
    try:
        other.wait_until_succeeds(f"curl -sf -m 5 -o /dev/null {CONSOLE}/", timeout=600)
    except Exception:
        print(guest_console(ALL[holder]))
        raise
    assert "pando-guest: console at" in guest_console(ALL[holder]), "the guest did not print its address"

with subtest("create a group through Pando's API"):
    sign_in(other)
    code, text = api(other, "POST", "/api/v1/groups", {"name": GROUP, "members": []})
    assert code in (200, 201), f"group create failed: {code} {text}"
    code, text = api(other, "GET", "/api/v1/groups")
    assert GROUP in text, f"new group not listed: {code} {text}"

with subtest("crash the node running the guest"):
    t0 = time.time()
    ALL[holder].crash()
    survivors = {n: m for n, m in ALL.items() if n != holder}

with subtest("the block and its volume primary re-converge on one survivor"):
    new_holder = None
    last_seen = {}
    deadline = time.time() + 300
    while time.time() < deadline and new_holder is None:
        cur = placement_nodes(get_json(next(iter(survivors.values())), NAME) or {})
        if len(cur) == 1 and holder not in cur:
            cand = next(iter(cur))
            last_seen = {"placement": cand, "role": role_of(ALL[cand], res)}
            if last_seen["role"] == "Primary":
                new_holder = cand
        else:
            last_seen = {"placement": sorted(cur)}
        time.sleep(2)
    assert new_holder, f"never re-converged on one survivor in 300s: {last_seen}"
    print(f"{NAME} re-converged on {new_holder} after {time.time() - t0:.1f}s")

with subtest("Pando answers again at the same address and kept its data"):
    third = next(m for n, m in survivors.items() if n != new_holder)
    deadline = time.time() + 600
    while time.time() < deadline and not console_up(third):
        time.sleep(2)
    if not console_up(third):
        print(guest_console(ALL[new_holder]))
        print(third.execute(f"ping -c3 -W1 {GUEST_IP}; ip neigh; curl -sv -m 5 {CONSOLE}/ 2>&1 | tail -20")[1])
        raise AssertionError("Pando's console never came back within 600s")
    print(f"Pando's console answered again {time.time() - t0:.1f}s after the crash")
    sign_in(third)
    code, text = api(third, "GET", "/api/v1/groups")
    assert GROUP in text, f"group lost across failover: {code} {text}"
    print("PANDO-GUEST DONE")
