"""share/nfs: an NFSv4.1 export that survives losing the node serving it.

Deploys a SINGLETON share/nfs block on a 3-way volume behind a VIP. The
client mounts VIP:/share (hard, NFSv4.1) and appends through one held fd; the
serving VM is crashed mid-write. The block, its DRBD primary and its VIP
must re-converge on one survivor, the same mount must resume without a
remount, and the host-side volume must match the client's checksum.

Runs after cluster-common.py (with client bound to n9), block-common.py
and vol_cluster.py.
"""

PORT = 2049  # client-facing, on the VIP
GANESHA_PORT = 12049  # Ganesha's own listen port: the VIP holder binds PORT
MOUNT = "/mnt/share-data"
VOL_NAME = "blk-default-files-files-data"  # storage.BlockVolumeName(ns, block, storageName)
STREAM_FILE = "/mnt/client/stream.txt"
MACHINES = {"n1": n1, "n2": n2, "n3": n3}

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: files
  namespace: default
spec:
  type: share/nfs
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
  storage:
    - name: files-data
      size: 64Mi
      replication: 3
      mountPath: {MOUNT}
  config:
    port: {GANESHA_PORT}
    gracePeriod: 30
  network:
    ports:
      - name: nfs
        port: {PORT}
        target_port: {GANESHA_PORT}
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: {GANESHA_PORT}
        period_seconds: 2
"""


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
    return m.execute("journalctl -u 'expanse-block-root@default-files-0.service' --no-pager -n 80 2>&1")[1]


form("nfs")
for m in MACHINES.values():
    wait_agent_ready(m)

with subtest("deploy a SINGLETON share/nfs block on a replicated volume"):
    deploy(n1, "files", MANIFEST)
    b = wait_phase(n1, "files", ["RUNNING"], 180)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"files placed on {nodes}: {b.get('status')}"
    holder = next(iter(nodes))
    VIP = wait_block_vip(n1, "files")

with subtest("every volume replica is UpToDate and the VIP sits on the holder"):
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")
    wait_for(lambda: vip_holders(VIP, NODES) == [holder], f"VIP {VIP} on {holder}", timeout=60)

with subtest("the client mounts VIP:/share over NFSv4.1 and writes (X3)"):
    client.succeed("mkdir -p /mnt/client")
    try:
        client.wait_until_succeeds(f"mount -t nfs4 -o vers=4.1,hard {VIP}:/share /mnt/client", timeout=90)
    except Exception:
        h = MACHINES[holder]
        print(client.execute(f"timeout 20 mount -v -t nfs4 -o vers=4.1,soft,retrans=1 {VIP}:/share /mnt/client 2>&1")[1])
        print(client.execute(f"timeout 5 bash -c '</dev/tcp/{VIP}/{PORT}' 2>&1; echo vip-port rc=$?")[1])
        print(client.execute(f"timeout 5 bash -c '</dev/tcp/{VIP}/{GANESHA_PORT}' 2>&1; echo direct rc=$?")[1])
        print(h.execute("ss -ltnp 2>&1 | grep -E '2049|12049'; ip -4 -o addr show eth1")[1])
        print(h.execute("journalctl -u expanse-agent --no-pager 2>&1 | grep -iE 'vip|lb |listener' | tail -20")[1])
        print(journal(h))
        raise
    client.succeed("echo -n hello-nfs > /mnt/client/hello.txt && sync")
    assert client.succeed("cat /mnt/client/hello.txt") == "hello-nfs"
    # Root is squashed by default, so the file belongs to nobody on the volume.
    owner = MACHINES[holder].succeed(f"stat -c %U {host_mount(MACHINES[holder])}/share/hello.txt").strip()
    assert owner == "nobody", f"root_squash not applied: owner {owner}"

with subtest("client-recovery state lives on the volume, not the node (X5)"):
    mnt = host_mount(MACHINES[holder])
    MACHINES[holder].wait_until_succeeds(f"ls {mnt}/.nfs-state/recovery/v4recov | grep -q .", timeout=30)

with subtest("a continuous writer runs through the mount"):
    client.succeed(
        "cat > /root/writer.sh << 'EOF'\n"
        "export PATH=/run/current-system/sw/bin:$PATH\n"
        # One descriptor held across the failover: only open-state reclaim keeps it valid.
        f"exec 3>> {STREAM_FILE}\n"
        "i=0\n"
        "while true; do\n"
        "  i=$((i+1))\n"
        "  if printf 'seq %06d\\n' \"$i\" >&3; then echo \"$i\" > /root/last_acked; fi\n"
        "  sleep 0.3\n"
        "done\n"
        "EOF\n"
    )
    client.succeed("systemd-run --unit=nfs-writer /bin/sh /root/writer.sh")
    wait_acked(5, 60, "pre-kill warmup")

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
        cur = placement_nodes(get_json(survivors[0], "files") or {})
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
    print(f"files re-converged on {new_holder} after {time.time() - t0:.1f}s")

with subtest("the same mount resumes writing without a remount (X4)"):
    try:
        resumed = wait_acked(acked_at_kill + 5, 240, "post-failover writes")
    except AssertionError:
        print(journal(MACHINES[new_holder]))
        print(client.execute("dmesg | grep -i nfs | tail -30")[1])
        raise
    # The survivor matched the client's recovery record despite its new source address.
    new_holder_m = MACHINES[new_holder]
    new_holder_m.succeed("journalctl -u 'expanse-block-root@default-files-0.service' --no-pager "
                         "| grep -q 'reclaim complete(1) clid count(1)'")
    print(f"writer resumed: {resumed} acked (was {acked_at_kill} at kill) "
          f"{time.time() - t0:.1f}s after the crash")

with subtest("the host volume agrees byte for byte with the client"):
    client.succeed("systemctl stop nfs-writer && sync")
    client_sum = client.succeed(f"sha256sum {STREAM_FILE}").split()[0]
    lines = client.succeed(f"wc -l < {STREAM_FILE}").strip()
    m = MACHINES[new_holder]
    mnt = host_mount(m)
    host_sum = m.succeed(f"sha256sum {mnt}/share/stream.txt").split()[0]
    assert host_sum == client_sum, f"checksum mismatch: client {client_sum}, volume {host_sum}"
    assert client.succeed("cat /mnt/client/hello.txt") == "hello-nfs", "pre-failover file lost"
    print(f"{lines} lines, sha256 {client_sum} on both sides")
