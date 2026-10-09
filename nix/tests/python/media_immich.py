"""media/immich: a photo library that keeps every acknowledged upload and queued job after losing its node.

Deploys a SINGLETON media/immich block on a 3-way volume behind a VIP on 80.
A client creates the administrator, uploads a photo that must get a thumbnail,
pauses metadata extraction, uploads more photos one acknowledged at a time and
the serving VM is crashed right after the last; the block, its volume and its
VIP must re-converge on a survivor that has every photo byte for byte, still
accepts the token issued before the crash, runs every job queued before it and
takes more uploads.

Runs after cluster-common.py (with client bound to n9), block-common.py and
vol_cluster.py.
"""

NAME = "photos"
MACHINES = {"n1": n1, "n2": n2, "n3": n3}
PHOTOS = 20
QUEUE = "metadataExtraction"

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {NAME}
  namespace: default
spec:
  type: media/immich
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
  config:
    machineLearning: false
  storage:
    - name: immich-data
      size: 2Gi
      replication: 3
      mountPath: /var/lib/immich
  network:
    ports:
      - name: http
        port: 80
        target_port: 2283
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 2283
        period_seconds: 2
"""


def vip_holders(vip, machines):
    """Nodes among machines carrying vip; never pass a crashed one (it would reboot)."""
    return [m.name for m in machines
            if m.execute(f"ip -4 -o addr show eth1 | grep -qF ' {vip}/'")[0] == 0]


def immich(*args):
    """One immich_client.py command against the VIP, decoded from its JSON output."""
    quoted = " ".join(f"'{a}'" for a in args)
    return json.loads(client.succeed(f"immich http://{VIP} admin@example.com Admin-password-4-tests {quoted}"))


def journal(m):
    return m.execute(f"journalctl -t 'expanse-block-default-{NAME}-0' --no-pager -n 200 2>&1")[1]


def wait_answering(m):
    try:
        client.wait_until_succeeds(f"immich http://{VIP} admin@example.com Admin-password-4-tests setup", timeout=300)
    except Exception:
        print(journal(m))
        raise


form("immich")
for m in MACHINES.values():
    wait_agent_ready(m)

with subtest("a manifest with a privileged port is rejected"):
    bad = MANIFEST.replace("    machineLearning: false", "    machineLearning: false\n    port: 80")
    n1.succeed(f"echo {base64.b64encode(bad.encode()).decode()} | base64 -d > /tmp/bad.yaml")
    out = n1.fail(f"expanse ctl block apply {SOCK} -f /tmp/bad.yaml 2>&1")
    assert "port" in out, f"rejection does not name port: {out}"

with subtest("deploy a SINGLETON media/immich block behind a VIP"):
    deploy(n1, NAME, MANIFEST)
    b = wait_phase(n1, NAME, ["RUNNING"], 300)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"{NAME} placed on {nodes}: {b.get('status')}"
    holder = next(iter(nodes))
    VIP = wait_block_vip(n1, NAME)
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")
    wait_for(lambda: vip_holders(VIP, NODES) == [holder], f"VIP {VIP} on {holder}", timeout=60)

with subtest("Immich answers with its database and job queue on the volume"):
    wait_answering(MACHINES[holder])
    assert "<html" in client.succeed(f"curl -sfL -m 10 http://{VIP}/").lower(), "the web UI is not served"
    vol = "/var/lib/expanse/volumes/*/mnt/immich"
    MACHINES[holder].succeed(f"test -s {vol}/postgres/PG_VERSION")
    MACHINES[holder].succeed(f"ls {vol}/redis/appendonlydir/ | grep -q '\\.aof$'")

with subtest("an uploaded photo gets its thumbnail"):
    token = immich("login")
    first = immich("upload", "warmup", "1")
    # Jobs wait for the first start's reverse-geocoding import (about 140 s in this VM).
    immich("wait-thumbs", ",".join(first), "600")

with subtest("crash the serving node right after photos are acknowledged"):
    immich("pause", QUEUE)
    uploaded = immich("upload", "photo", str(PHOTOS))
    waiting = immich("queue", QUEUE)["statistics"]
    assert waiting["paused"] + waiting["waiting"] == PHOTOS, f"not every upload left a queued job: {waiting}"
    t0 = time.time()
    MACHINES[holder].crash()
    survivors = [m for n, m in MACHINES.items() if n != holder]

with subtest("block, volume primary and VIP re-converge on one survivor"):
    new_holder = None
    last_seen = {}
    deadline = time.time() + 240
    while time.time() < deadline and new_holder is None:
        cur = placement_nodes(get_json(survivors[0], NAME) or {})
        if len(cur) == 1 and holder not in cur:
            cand = next(iter(cur))
            last_seen = {"placement": cand, "role": role_of(MACHINES[cand], res),
                         "vip": vip_holders(VIP, survivors)}
            if last_seen["role"] == "Primary" and last_seen["vip"] == [cand]:
                new_holder = cand
        else:
            last_seen = {"placement": sorted(cur)}
        time.sleep(2)
    assert new_holder, f"never re-converged on one survivor in 240s: {last_seen}"
    print(f"{NAME} re-converged on {new_holder} after {time.time() - t0:.1f}s")

with subtest("the survivor has every acknowledged photo, byte for byte, and honours the pre-crash token"):
    wait_answering(MACHINES[new_holder])
    print(f"Immich answered again {time.time() - t0:.1f}s after the crash")
    assets = immich("assets")
    missing = sorted(i for i, sha in uploaded.items() if assets.get(i) != sha)
    assert not missing, f"{len(missing)} of {PHOTOS} acknowledged photos lost across failover: {missing}"
    originals = immich("original-sha1", ",".join(uploaded))
    damaged = sorted(i for i, sha in uploaded.items() if originals.get(i) != sha)
    assert not damaged, f"{len(damaged)} of {PHOTOS} originals differ after failover: {damaged}"
    assert immich("token-ok", token), "the token issued before the crash was refused"

with subtest("every job queued before the crash runs on the survivor"):
    immich("resume", QUEUE)
    immich("wait-thumbs", ",".join(uploaded), "180")

with subtest("the survivor takes more uploads"):
    after = immich("upload", "after-failover", "1")
    immich("wait-thumbs", ",".join(after), "120")
    print("MEDIA-IMMICH DONE")
