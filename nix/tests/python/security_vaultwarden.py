"""security/vaultwarden: a password manager that keeps every acknowledged vault item after losing its node.

Deploys a SINGLETON security/vaultwarden block on a 3-way volume behind a VIP
on 80, with sign-ups closed except for one domain. A client registers, saves
vault items and the serving VM is crashed right after the last is acknowledged;
the block, its volume and its VIP must re-converge on a survivor that has every
item, still accepts the login token issued before the crash, and saves more.

Runs after cluster-common.py (with client bound to n9), block-common.py and
vol_cluster.py.
"""

NAME = "vault"
MACHINES = {"n1": n1, "n2": n2, "n3": n3}
EMAIL = "alice@vault.test"
# The server only stores a hash of what the client sends; real clients derive it from the master password.
PW_HASH = base64.b64encode(b"client-side-master-password-hash").decode()
ENC = "2.QUFBQUFBQUFBQUFBQUFBQQ==|QkJCQkJCQkJCQkJCQkJCQg==|Q0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0M="
ITEMS = 20

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {NAME}
  namespace: default
spec:
  type: security/vaultwarden
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 200m
      memory: 128Mi
  storage:
    - name: vaultwarden-data
      size: 256Mi
      replication: 3
      mountPath: /var/lib/vaultwarden
  config:
    domain: http://vault.test
    adminToken: admin-token-for-tests
    settings:
      SIGNUPS_DOMAINS_WHITELIST: vault.test
  network:
    ports:
      - name: http
        port: 80
        target_port: 18000
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 18000
        period_seconds: 2
"""


def vip_holders(vip, machines):
    """Nodes among machines carrying vip; never pass a crashed one (it would reboot)."""
    return [m.name for m in machines
            if m.execute(f"ip -4 -o addr show eth1 | grep -qF ' {vip}/'")[0] == 0]


def http(method, path, body=None, token=None, form_body=None):
    """One request through the VIP; returns (status, body). JSON bodies travel base64-encoded past the shell."""
    args = f"-X {method}"
    if token:
        args += f" -H 'Authorization: Bearer {token}'"
    if body is not None:
        client.succeed(f"echo {base64.b64encode(json.dumps(body).encode()).decode()} | base64 -d > /tmp/body")
        args += " -H 'Content-Type: application/json' --data-binary @/tmp/body"
    if form_body is not None:
        args += "".join(f" --data-urlencode '{k}={v}'" for k, v in form_body.items())
    out = client.succeed(f"curl -s -m 10 -w '\\n%{{http_code}}' {args} 'http://{VIP}{path}'")
    text, _, code = out.rpartition("\n")
    return int(code), text


def register(email):
    return http("POST", "/identity/accounts/register", {
        "email": email, "name": email.split("@")[0], "masterPasswordHash": PW_HASH, "key": ENC,
        "kdf": 0, "kdfIterations": 600000,
    })


def log_in():
    code, body = http("POST", "/identity/connect/token", form_body={
        "grant_type": "password", "username": EMAIL, "password": PW_HASH, "scope": "api offline_access",
        "client_id": "cli", "device_type": "8", "device_name": "expanse-test",
        "device_identifier": "5a1e7c3e-1b7a-4c5e-9d1e-0c9b1f7e2a10",
    })
    assert code == 200, f"login answered {code}: {body}"
    return json.loads(body)["access_token"]


def save_item(token, n):
    code, body = http("POST", "/api/ciphers", {"type": 2, "name": ENC, "notes": ENC,
                                               "secureNote": {"type": 0}, "favorite": n % 2 == 0}, token)
    assert code == 200, f"saving item {n} answered {code}: {body}"
    return json.loads(body)["id"]


def item_ids(token):
    code, body = http("GET", "/api/ciphers", token=token)
    assert code == 200, f"listing items answered {code}: {body}"
    return {c["id"] for c in json.loads(body)["data"]}


def journal(m):
    return m.execute(f"journalctl -u 'expanse-block@default-{NAME}-0.service' --no-pager -n 80 2>&1")[1]


form("vaultwarden")
for m in MACHINES.values():
    wait_agent_ready(m)

with subtest("a manifest without a domain is rejected"):
    bad = MANIFEST.replace("    domain: http://vault.test\n", "")
    n1.succeed(f"echo {base64.b64encode(bad.encode()).decode()} | base64 -d > /tmp/bad.yaml")
    out = n1.fail(f"expanse ctl block apply {SOCK} -f /tmp/bad.yaml 2>&1")
    assert "domain" in out, f"rejection does not name domain: {out}"

with subtest("deploy a SINGLETON security/vaultwarden block behind a VIP"):
    deploy(n1, NAME, MANIFEST)
    b = wait_phase(n1, NAME, ["RUNNING"], 240)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"{NAME} placed on {nodes}: {b.get('status')}"
    holder = next(iter(nodes))
    VIP = wait_block_vip(n1, NAME)
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")
    wait_for(lambda: vip_holders(VIP, NODES) == [holder], f"VIP {VIP} on {holder}", timeout=60)

with subtest("the API, web vault and admin page answer; sign-ups follow the settings"):
    try:
        client.wait_until_succeeds(f"curl -sf -m 5 http://{VIP}/alive", timeout=120)
    except Exception:
        print(journal(MACHINES[holder]))
        raise
    assert "<html" in client.succeed(f"curl -sf -m 5 http://{VIP}/").lower(), "the web vault is not served"
    code, _ = http("GET", "/admin")
    assert code == 200, f"/admin answered {code} with adminToken set"
    code, body = register("mallory@other.test")
    assert code >= 400, f"a sign-up outside SIGNUPS_DOMAINS_WHITELIST answered {code}: {body}"
    code, body = register(EMAIL)
    assert code == 200, f"a whitelisted sign-up answered {code}: {body}"
    token = log_in()

with subtest("crash the serving node right after vault items are acknowledged"):
    saved = [save_item(token, n) for n in range(ITEMS)]
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

with subtest("the survivor has every acknowledged item and honours the pre-crash token"):
    try:
        client.wait_until_succeeds(f"curl -sf -m 5 http://{VIP}/alive", timeout=180)
    except Exception:
        print(journal(MACHINES[new_holder]))
        raise
    print(f"Vaultwarden answered again {time.time() - t0:.1f}s after the crash")
    missing = set(saved) - item_ids(token)
    assert not missing, f"{len(missing)} of {ITEMS} acknowledged items lost across failover: {sorted(missing)}"

with subtest("the survivor still saves items"):
    after = save_item(token, ITEMS)
    assert item_ids(token) == set(saved) | {after}
    print("SECURITY-VAULTWARDEN DONE")
