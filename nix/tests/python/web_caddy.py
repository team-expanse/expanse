"""web/caddy: a Caddy that keeps serving, with the same certificate, after losing its node.

Deploys a SINGLETON web/caddy block on a 3-way volume behind a VIP that exposes
both 80 and 443. One site proxies to a backend on the client with a certificate
from Caddy's internal CA, another answers plain HTTP. The serving VM is crashed;
the block, its volume and its VIP must re-converge on a survivor, which must
serve the same certificate from the volume.

Runs after cluster-common.py (with client bound to n9), block-common.py and
vol_cluster.py. Expects TAG_BACKEND (web-caddy.nix) spliced in ahead of this file.
"""

NAME = "edge"
VOL_NAME = f"blk-default-{NAME}-caddy-data"  # storage.BlockVolumeName(ns, block, storageName)
MACHINES = {"n1": n1, "n2": n2, "n3": n3}

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {NAME}
  namespace: default
spec:
  type: web/caddy
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
  storage:
    - name: caddy-data
      size: 128Mi
      replication: 3
      mountPath: /var/lib/caddy
  config:
    sites:
      - host: app.test
        tls: internal
        reverseProxy: ["n9:8000"]
      - host: plain.test
        tls: "off"
        respond: plain-ok
  network:
    ports:
      - name: http
        port: 80
        target_port: 18080
        protocol: tcp
        expose: EXPOSE_VIP
      - name: https
        port: 443
        target_port: 18443
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 18080
        period_seconds: 2
"""


def vip_holders(vip, machines):
    """Nodes among machines carrying vip; never pass a crashed one (it would reboot)."""
    return [m.name for m in machines
            if m.execute(f"ip -4 -o addr show eth1 | grep -qF ' {vip}/'")[0] == 0]


def host_mount(m):
    row = volume_row(m, VOL_NAME)
    return f"/var/lib/expanse/volumes/{row['id']}/mnt"


def https(path="/"):
    return f"curl -sfk -m 5 --resolve app.test:443:{VIP} https://app.test{path}"


def fingerprint():
    """SHA-256 fingerprint and issuer of the certificate served for app.test on VIP:443."""
    return client.succeed(
        f"echo | openssl s_client -connect {VIP}:443 -servername app.test 2>/dev/null "
        "| openssl x509 -noout -fingerprint -sha256 -issuer").strip()


def journal(m):
    return m.execute(f"journalctl -u 'expanse-block@default-{NAME}-0.service' --no-pager -n 60 2>&1")[1]


client.succeed(f"systemd-run --unit=backend python3 {TAG_BACKEND} 8000 caddy-backend")
client.wait_for_open_port(8000)

form("caddy")
for m in MACHINES.values():
    wait_agent_ready(m)

with subtest("a manifest with both sites and a caddyfile is rejected"):
    bad = MANIFEST.replace("  config:\n", "  config:\n    caddyfile: ':80'\n")
    n1.succeed(f"echo {base64.b64encode(bad.encode()).decode()} | base64 -d > /tmp/bad.yaml")
    out = n1.fail(f"expanse ctl block apply {SOCK} -f /tmp/bad.yaml 2>&1")
    assert "sites" in out, f"rejection does not name sites: {out}"

with subtest("deploy a SINGLETON web/caddy block exposing 80 and 443 on one VIP"):
    deploy(n1, NAME, MANIFEST)
    b = wait_phase(n1, NAME, ["RUNNING"], 180)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"{NAME} placed on {nodes}: {b.get('status')}"
    holder = next(iter(nodes))
    VIP = wait_block_vip(n1, NAME)
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")
    wait_for(lambda: vip_holders(VIP, NODES) == [holder], f"VIP {VIP} on {holder}", timeout=60)

with subtest("HTTPS proxies to the backend with an internal-CA certificate"):
    try:
        client.wait_until_succeeds(https(), timeout=120)
    except Exception:
        print(client.execute(https() + " -v 2>&1 | tail -20")[1])
        print(journal(MACHINES[holder]))
        raise
    body = client.succeed(https())
    assert body.startswith("caddy-backend xff="), f"unexpected body: {body!r}"
    assert "192.168.1." in body, f"Caddy did not add X-Forwarded-For: {body!r}"
    cert_before = fingerprint()
    assert "Caddy Local Authority" in cert_before, f"not an internal-CA certificate: {cert_before}"

with subtest("plain HTTP redirects to port 443, not Caddy's own 18443"):
    out = client.succeed(f"curl -s -o /dev/null -m 5 -w '%{{http_code}} %{{redirect_url}}' "
                         f"--resolve app.test:80:{VIP} http://app.test/x?y=1")
    assert out == "308 https://app.test/x?y=1", f"redirect: {out!r}"
    out = client.succeed(f"curl -sf -m 5 --resolve plain.test:80:{VIP} http://plain.test/")
    assert out == "plain-ok", f"plain site: {out!r}"

with subtest("the certificate and the internal CA live on the volume"):
    mnt = host_mount(MACHINES[holder])
    root_before = MACHINES[holder].succeed(f"sha256sum < {mnt}/caddy/pki/authorities/local/root.crt")
    MACHINES[holder].succeed(f"test -s {mnt}/caddy/certificates/local/app.test/app.test.crt")

with subtest("crash the serving node"):
    # Caddy never fsyncs; expanse-block-run syncs the volume every 2 s (docs/CADDY.md).
    time.sleep(3)
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

with subtest("the survivor serves the same certificate and both sites"):
    try:
        client.wait_until_succeeds(https(), timeout=120)
    except Exception:
        print(journal(MACHINES[new_holder]))
        raise
    print(f"HTTPS answered again {time.time() - t0:.1f}s after the crash")
    cert_after = fingerprint()
    assert cert_after == cert_before, f"certificate changed across failover:\n{cert_before}\n{cert_after}"
    out = client.succeed(f"curl -sf -m 5 --resolve plain.test:80:{VIP} http://plain.test/")
    assert out == "plain-ok", f"plain site after failover: {out!r}"
    mnt = host_mount(MACHINES[new_holder])
    root_after = MACHINES[new_holder].succeed(f"sha256sum < {mnt}/caddy/pki/authorities/local/root.crt")
    assert root_after == root_before, "Caddy's internal CA changed across failover"
    print("WEB-CADDY DONE")
