"""storage/s3: S3 object storage that survives losing the node serving it.

Deploys a SINGLETON storage/s3 (Garage) block on a 3-way volume behind a
VIP. The client signs requests with curl --aws-sigv4, stores a large object
and runs a writer that PUTs one numbered object at a time; the serving VM
is crashed mid-write. The block, its DRBD primary and its VIP must
re-converge on one survivor, the writer must resume, and every object the
client saw acknowledged must read back intact.

Runs after cluster-common.py (with client bound to n9), block-common.py
and vol_cluster.py.
"""

PORT = 3900  # client-facing, on the VIP
GARAGE_PORT = 13900  # Garage's own S3 port: the VIP holder binds PORT
KEY_ID = "GK0123456789abcdef01234567"
SECRET = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
VOL_NAME = "blk-default-objects-objects-data"  # storage.BlockVolumeName(ns, block, storageName)
MACHINES = {"n1": n1, "n2": n2, "n3": n3}

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: objects
  namespace: default
spec:
  type: storage/s3
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
  storage:
    - name: objects-data
      size: 128Mi
      replication: 3
      mountPath: /var/lib/s3
  config:
    port: {GARAGE_PORT}
    accessKeyId: {KEY_ID}
    secretAccessKey: {SECRET}
    buckets: [media]
  network:
    ports:
      - name: s3
        port: {PORT}
        target_port: {GARAGE_PORT}
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: {GARAGE_PORT}
        period_seconds: 2
"""


def s3(args, vip=None):
    """A signed curl invocation against the block's VIP."""
    return (f"curl -sS -f --max-time 10 --aws-sigv4 aws:amz:garage:s3 --user {KEY_ID}:{SECRET} "
            f"{args} http://{vip or VIP}:{PORT}")


def vip_holders(vip, machines):
    """Nodes among machines carrying vip; never pass a crashed one (it would reboot)."""
    return [m.name for m in machines
            if m.execute(f"ip -4 -o addr show eth1 | grep -qF ' {vip}/'")[0] == 0]


def host_mount(m):
    row = volume_row(m, VOL_NAME)
    return f"/var/lib/expanse/volumes/{row['id']}/mnt" if row else None


def last_acked():
    out = client.execute("cat /root/last_acked 2>/dev/null || echo 0")[1].strip()
    return int(out) if out.isdigit() else 0


def wait_acked(n, timeout, what):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if last_acked() >= n:
            return last_acked()
        time.sleep(1)
    raise AssertionError(f"{what}: only {last_acked()} acked writes in {timeout}s (want >= {n})")


def journal(m):
    return m.execute("journalctl -u 'expanse-block@default-objects-0.service' --no-pager -n 80 2>&1")[1]


form("s3")
for m in MACHINES.values():
    wait_agent_ready(m)

with subtest("a manifest without the access key is rejected"):
    bad = MANIFEST.replace(f"    accessKeyId: {KEY_ID}\n", "")
    n1.succeed(f"echo {base64.b64encode(bad.encode()).decode()} | base64 -d > /tmp/bad.yaml")
    out = n1.fail(f"expanse ctl block apply {SOCK} -f /tmp/bad.yaml 2>&1")
    assert "accessKeyId" in out, f"rejection does not name accessKeyId: {out}"

with subtest("deploy a SINGLETON storage/s3 block on a replicated volume"):
    deploy(n1, "objects", MANIFEST)
    b = wait_phase(n1, "objects", ["RUNNING"], 180)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"objects placed on {nodes}: {b.get('status')}"
    holder = next(iter(nodes))
    VIP = wait_block_vip(n1, "objects")

with subtest("every volume replica is UpToDate and the VIP sits on the holder"):
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")
    wait_for(lambda: vip_holders(VIP, NODES) == [holder], f"VIP {VIP} on {holder}", timeout=60)

with subtest("the client stores and reads objects through the VIP"):
    try:
        client.wait_until_succeeds(f"echo -n hello-s3 | {s3('-T -')}/media/hello.txt", timeout=90)
    except Exception:
        h = MACHINES[holder]
        print(client.execute(f"echo x | {s3('-v -T -')}/media/hello.txt 2>&1")[1])
        print(h.execute(f"ss -ltnp 2>&1 | grep -E '{PORT}|{GARAGE_PORT}'; ip -4 -o addr show eth1")[1])
        print(journal(h))
        raise
    assert client.succeed(s3("") + "/media/hello.txt") == "hello-s3"
    client.succeed("head -c 8388608 /dev/urandom > /root/big.bin")
    client.succeed(s3("-T /root/big.bin") + "/media/big.bin")
    BIG_SUM = client.succeed("sha256sum < /root/big.bin").split()[0]
    # Unsigned requests are refused: the bucket is private to the key.
    client.fail(f"curl -sS -f --max-time 10 http://{VIP}:{PORT}/media/hello.txt")

with subtest("the key may create its own buckets"):
    client.succeed(s3("-X PUT") + "/scratch")
    client.succeed(f"echo -n mine | {s3('-T -')}/scratch/a.txt")

with subtest("Garage's metadata, objects and secrets live on the volume"):
    mnt = host_mount(MACHINES[holder])
    MACHINES[holder].succeed(f"test -s {mnt}/.garage/meta/node_key")
    MACHINES[holder].succeed(f"test -s {mnt}/.garage/secrets/rpc_secret")
    MACHINES[holder].succeed(f"find {mnt}/.garage/data -type f | grep -q .")

with subtest("a continuous writer PUTs numbered objects"):
    client.succeed(
        "cat > /root/writer.sh << 'EOF'\n"
        "export PATH=/run/current-system/sw/bin:$PATH\n"
        "i=1\n"
        "while true; do\n"
        "  n=$(printf %06d $i)\n"
        # Only a 2xx counts; a failed PUT is retried under the same number.
        f"  if printf 'seq %s\\n' $n | {s3('-T -')}/media/seq/$n; then\n"
        "    echo $i > /root/last_acked; i=$((i+1))\n"
        "  fi\n"
        "  sleep 0.3\n"
        "done\n"
        "EOF\n"
    )
    client.succeed("systemd-run --unit=s3-writer /bin/sh /root/writer.sh")
    wait_acked(20, 90, "pre-kill warmup")

with subtest("kill the serving node's VM mid-write"):
    acked_at_kill = last_acked()
    t0 = time.time()
    MACHINES[holder].crash()
    survivors = [m for n, m in MACHINES.items() if n != holder]

with subtest("block, volume primary and VIP re-converge on one survivor"):
    new_holder = None
    last_seen = {}
    deadline = time.time() + 240
    while time.time() < deadline and new_holder is None:
        cur = placement_nodes(get_json(survivors[0], "objects") or {})
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
    print(f"objects re-converged on {new_holder} after {time.time() - t0:.1f}s")

with subtest("the writer resumes against the same endpoint and key"):
    try:
        resumed = wait_acked(acked_at_kill + 5, 240, "post-failover writes")
    except AssertionError:
        print(journal(MACHINES[new_holder]))
        raise
    print(f"writer resumed: {resumed} acked (was {acked_at_kill} at kill) "
          f"{time.time() - t0:.1f}s after the crash")

with subtest("every acknowledged object reads back intact"):
    client.succeed("systemctl stop s3-writer")
    acked = last_acked()
    client.succeed(
        f"for i in $(seq 1 {acked}); do n=$(printf %06d $i); "
        f"[ \"$({s3('')}/media/seq/$n)\" = \"seq $n\" ] || {{ echo missing $n; exit 1; }}; done"
    )
    assert client.succeed(s3("") + "/media/hello.txt") == "hello-s3", "pre-failover object lost"
    got = client.succeed(s3("") + "/media/big.bin | sha256sum").split()[0]
    assert got == BIG_SUM, f"big.bin checksum {got}, want {BIG_SUM}"
    assert client.succeed(s3("") + "/scratch/a.txt") == "mine", "key-created bucket lost"
    print(f"{acked} acknowledged objects and the 8 MiB object intact after failover")
