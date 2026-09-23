"""PHASE-03-TASKS.md Stream B1: a SINGLETON share/smb block on a
DRBD-backed volume, mounted by an external CIFS client through the
block's VIP. Proves X1 (deploy + client write) — X2 (kill-mid-write
failover) is B2's own test, not this one.

Runs after cluster-common.py, client-common.py (with `client` bound to
the external VM) and block-common.py. Expects VIP_POOL (two addresses,
share-smb.nix) spliced in ahead of this file.
"""

PORT = 445  # the client-facing, VIP-exposed port — SMB clients assume it
# smbd's own listen port must differ from PORT: the VIP holder's own
# listen check reserves PORT on whichever node it moves to, which
# collides ("address already in use") with smbd already bound to
# 0.0.0.0:PORT on the very node the VIP is trying to move TO (the
# block's own host) — the same reason every other EXPOSE_VIP block
# (nginx: exposed 80, internal 8080) already keeps the two apart.
SMBD_PORT = 44445
SHARE = "testshare"
MOUNT = "/mnt/share-data"

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: share
  namespace: default
spec:
  type: share/smb
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
  storage:
    - name: share-data
      size: 64Mi
      replication: 3
      mountPath: {MOUNT}
  config:
    port: {SMBD_PORT}
    shareName: {SHARE}
    guestOk: true
  network:
    ports:
      - name: smb
        port: {PORT}
        target_port: {SMBD_PORT}
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: {SMBD_PORT}
        period_seconds: 2
"""

def vip_holders(vip_addr):
    """Nodes currently carrying vip_addr on eth1."""
    holders = []
    for m in [n1, n2, n3]:
        rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {vip_addr} || true")
        if rc == 0 and out.strip():
            holders.append(m.name)
    return holders


def wait_single_holder(vip_addr, timeout):
    deadline = time.time() + timeout
    holders = []
    while time.time() < deadline:
        holders = vip_holders(vip_addr)
        if len(holders) == 1:
            return holders[0]
        time.sleep(2)
    raise AssertionError(f"{vip_addr}: never settled on exactly 1 holder (last: {holders})")


form("smb")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)

with subtest("the cluster's own management UI claims one pool address"):
    # internal/agent/ui_vip.go allocates unconditionally on every
    # node.enable=true agent, independent of any block — the pool must
    # have a free address left over once it has, or the block's own VIP
    # request starves (vip:no free address in pool). Allocation order
    # within the pool is not this test's concern, so poll both addresses
    # and take whichever one settles first.
    deadline = time.time() + 60
    ui_vip = None
    while time.time() < deadline and ui_vip is None:
        for pool_addr in VIP_POOL:
            if len(vip_holders(pool_addr)) == 1:
                ui_vip = pool_addr
                break
        if ui_vip is None:
            time.sleep(2)
    assert ui_vip, f"no VIP claimed within 60 s (expected the UI to take one): {VIP_POOL}"

with subtest("deploy a SINGLETON share/smb block bound to a volume"):
    # Generous budget: the volume must be requested, placed and
    # replicated to a healthy state (P12's gate) before the block can
    # schedule at all — a real bootstrapping chain, not a container start.
    deploy(n1, "share", MANIFEST)
    b = wait_phase(n1, "share", ["RUNNING"], 180)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"share placed on {nodes}, want exactly 1: {b.get('status')}"

with subtest("the block claims the other pool address as its own VIP"):
    remaining = [a for a in VIP_POOL if a != ui_vip]
    assert len(remaining) == 1, f"VIP_POOL must have exactly 2 addresses: {VIP_POOL}"
    VIP = remaining[0]
    # wait_single_holder alone can return early on a stale/in-flight
    # handover (the address briefly on the wrong node while the mesh
    # converges) — poll for the specific node the block actually placed
    # on, not just "any single holder".
    deadline = time.time() + 60
    holder = None
    while time.time() < deadline:
        hs = vip_holders(VIP)
        if len(hs) == 1 and hs[0] in nodes:
            holder = hs[0]
            break
        time.sleep(2)
    assert holder, f"share's VIP ({VIP}) never settled on its own placement {nodes} (last holders: {vip_holders(VIP)})"

with subtest("the external client mounts the share and writes a file"):
    client.succeed("mkdir -p /mnt/client")
    client.wait_until_succeeds(
        f"mount -t cifs //{VIP}/{SHARE} /mnt/client -o guest",
        timeout=60,
    )
    client.succeed("echo -n hello-smb-share > /mnt/client/hello.txt && sync")
    ref = client.succeed("sha256sum /mnt/client/hello.txt").split()[0]

with subtest("content and checksum verify from a second, independent read"):
    client.succeed("umount /mnt/client")
    client.succeed(f"mount -t cifs //{VIP}/{SHARE} /mnt/client -o guest")
    got = client.succeed("sha256sum /mnt/client/hello.txt").split()[0]
    assert got == ref, f"checksum mismatch on second read: {got} != {ref}"
    content = client.succeed("cat /mnt/client/hello.txt")
    assert content == "hello-smb-share", f"content mismatch: {content!r}"

print("SHARE-SMB DONE")
