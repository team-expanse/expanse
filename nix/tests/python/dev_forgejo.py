"""dev/forgejo: a Forgejo that keeps every acknowledged push and issue after losing its node.

Deploys a SINGLETON dev/forgejo block on a 3-way volume behind a VIP exposing
HTTP on 80 and SSH on 2222. The client pushes over both, opens an issue, pushes
once more and the serving VM is crashed at once; the block, its volume and its
VIP must re-converge on a survivor that has every commit and the issue, the same
SSH host key, and still accepts pushes.

Runs after cluster-common.py (with client bound to n9), block-common.py and
vol_cluster.py.
"""

NAME = "forge"
MACHINES = {"n1": n1, "n2": n2, "n3": n3}
ADMIN, PASSWORD = "gitadmin", "forge-pass-123"
REPO = f"{ADMIN}/demo"

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {NAME}
  namespace: default
spec:
  type: dev/forgejo
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 200m
      memory: 256Mi
  storage:
    - name: forgejo-data
      size: 512Mi
      replication: 3
      mountPath: /var/lib/forgejo
  config:
    rootURL: http://git.test/
    adminUser: {ADMIN}
    adminPassword: {PASSWORD}
    adminEmail: ops@git.test
    settings:
      repository:
        DEFAULT_BRANCH: trunk
  network:
    ports:
      - name: http
        port: 80
        target_port: 13000
        protocol: tcp
        expose: EXPOSE_VIP
      - name: ssh
        port: 2222
        target_port: 12222
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 13000
        period_seconds: 2
"""


def vip_holders(vip, machines):
    """Nodes among machines carrying vip; never pass a crashed one (it would reboot)."""
    return [m.name for m in machines
            if m.execute(f"ip -4 -o addr show eth1 | grep -qF ' {vip}/'")[0] == 0]


def api(path, data=None):
    """Forgejo's API through the VIP as the admin; returns the parsed JSON."""
    body = f"-H 'Content-Type: application/json' -d '{json.dumps(data)}'" if data is not None else ""
    return json.loads(client.succeed(f"curl -sf -m 10 -u {ADMIN}:{PASSWORD} {body} 'http://{VIP}/api/v1{path}'"))


def git(cmd):
    return client.succeed(f"cd /root/src && {cmd}")


def commit_and_push(msg, remote):
    git(f"git commit -q --allow-empty -m {msg} && git push -q {remote} trunk 2>&1")


def ssh_remote():
    return f"ssh://git@{VIP}:2222/{REPO}.git"


def host_key():
    return client.succeed(f"ssh-keyscan -p 2222 -t rsa {VIP} 2>/dev/null | cut -d' ' -f2-").strip()


def log_in():
    """Log in through the web form, keeping the session cookie in /root/jar."""
    # Forgejo 16's form carries no CSRF token; it checks the request's origin instead.
    code = client.succeed(f"curl -s -m 10 -o /dev/null -w '%{{http_code}}' -c /root/jar -b /root/jar "
                          f"-d user_name={ADMIN} -d password={PASSWORD} http://{VIP}/user/login")
    assert code == "303", f"web login answered {code}"


def logged_in():
    code = client.succeed(f"curl -s -m 10 -o /dev/null -w '%{{http_code}}' -b /root/jar http://{VIP}/user/settings")
    return code == "200"


def journal(m):
    return m.execute(f"journalctl -u 'expanse-block@default-{NAME}-0.service' --no-pager -n 80 2>&1")[1]


form("forgejo")
for m in MACHINES.values():
    wait_agent_ready(m)

with subtest("a manifest without an admin password is rejected"):
    bad = MANIFEST.replace(f"    adminPassword: {PASSWORD}\n", "")
    n1.succeed(f"echo {base64.b64encode(bad.encode()).decode()} | base64 -d > /tmp/bad.yaml")
    out = n1.fail(f"expanse ctl block apply {SOCK} -f /tmp/bad.yaml 2>&1")
    assert "adminPassword" in out, f"rejection does not name adminPassword: {out}"

with subtest("deploy a SINGLETON dev/forgejo block exposing HTTP and SSH on one VIP"):
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

with subtest("the admin exists, registration is closed and settings apply"):
    try:
        client.wait_until_succeeds(f"curl -sf -m 5 -u {ADMIN}:{PASSWORD} http://{VIP}/api/v1/user", timeout=120)
    except Exception:
        print(journal(MACHINES[holder]))
        raise
    assert api("/user")["is_admin"], "the configured admin is not an administrator"
    page = client.succeed(f"curl -s -m 5 http://{VIP}/user/sign_up")
    assert "Registration is disabled" in page, "registration is open"
    repo = api("/user/repos", {"name": "demo"})
    assert repo["default_branch"] == "trunk", f"settings.repository not applied: {repo['default_branch']}"
    assert repo["ssh_url"] == f"ssh://git@git.test:2222/{REPO}.git", f"clone URL: {repo['ssh_url']}"

with subtest("push over SSH and HTTP through the VIP"):
    client.succeed("mkdir -p -m 700 /root/.ssh && ssh-keygen -q -t ed25519 -N '' -f /root/.ssh/id_ed25519")
    api("/user/keys", {"title": "client", "key": client.succeed("cat /root/.ssh/id_ed25519.pub").strip()})
    client.succeed("git init -q -b trunk /root/src")
    commit_and_push("one", ssh_remote())
    commit_and_push("two", f"http://{ADMIN}:{PASSWORD}@{VIP}/{REPO}.git")
    key_before = host_key()
    log_in()
    assert logged_in(), "the web session does not work"

with subtest("crash the serving node right after an issue and a push are acknowledged"):
    issue = api(f"/repos/{REPO}/issues", {"title": "filed-before-crash"})
    commit_and_push("three", ssh_remote())
    head = git("git rev-parse HEAD").strip()
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

with subtest("the survivor has every acknowledged commit, the issue and the same host key"):
    try:
        client.wait_until_succeeds(f"curl -sf -m 5 http://{VIP}/api/v1/version", timeout=180)
    except Exception:
        print(journal(MACHINES[new_holder]))
        raise
    print(f"Forgejo answered again {time.time() - t0:.1f}s after the crash")
    client.succeed(f"git clone -q {ssh_remote()} /root/clone && git -C /root/clone fsck --strict")
    got = client.succeed("git -C /root/clone rev-parse HEAD").strip()
    assert got == head, f"HEAD after failover is {got}, the last acknowledged push was {head}"
    history = client.succeed("git -C /root/clone log --format=%s").split()
    assert history == ["three", "two", "one"], f"history after failover: {history}"
    assert api(f"/repos/{REPO}/issues/{issue['number']}")["title"] == "filed-before-crash"
    assert host_key() == key_before, "the SSH host key changed across failover"
    assert logged_in(), "the web session was lost across failover"

with subtest("the survivor still accepts pushes"):
    commit_and_push("four", ssh_remote())
    commits = api(f"/repos/{REPO}/commits?sha=trunk&limit=1")
    assert commits[0]["commit"]["message"].strip() == "four", f"latest commit: {commits[0]['commit']['message']!r}"
    print("DEV-FORGEJO DONE")
