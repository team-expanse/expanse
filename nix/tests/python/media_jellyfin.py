"""media/jellyfin: a media server that keeps every acknowledged change after losing its node.

Deploys a SINGLETON media/jellyfin block on a 3-way volume behind a VIP on 80.
A client completes the startup wizard, adds a movie library from the nodes'
/srv/media, streams the movie, creates users and the serving VM is crashed right
after the last is acknowledged; the block, its volume and its VIP must
re-converge on a survivor that has every user and the library, still accepts the
token issued before the crash, and saves more.

Runs after cluster-common.py (with client bound to n9), block-common.py and
vol_cluster.py.
"""

NAME = "media"
MACHINES = {"n1": n1, "n2": n2, "n3": n3}
ADMIN, ADMIN_PW = "admin", "admin-password-for-tests"
USERS = 20
AUTH = 'MediaBrowser Client="expanse-test", Device="n9", DeviceId="expanse-test-n9", Version="1.0"'

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {NAME}
  namespace: default
spec:
  type: media/jellyfin
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 500m
      memory: 512Mi
  storage:
    - name: jellyfin-data
      size: 3Gi
      replication: 3
      mountPath: /var/lib/jellyfin
  config:
    publishedServerUrl: http://media.test
  network:
    ports:
      - name: http
        port: 80
        target_port: 8096
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 8096
        period_seconds: 2
"""


def vip_holders(vip, machines):
    """Nodes among machines carrying vip; never pass a crashed one (it would reboot)."""
    return [m.name for m in machines
            if m.execute(f"ip -4 -o addr show eth1 | grep -qF ' {vip}/'")[0] == 0]


def http(method, path, body=None, token=None, out="/dev/stdout"):
    """One request through the VIP; returns (status, body). JSON bodies travel base64-encoded past the shell."""
    auth = AUTH + (f', Token="{token}"' if token else "")
    args = f"-X {method} -H 'Authorization: {auth}'"
    if body is not None:
        client.succeed(f"echo {base64.b64encode(json.dumps(body).encode()).decode()} | base64 -d > /tmp/body")
        args += " -H 'Content-Type: application/json' --data-binary @/tmp/body"
    res = client.succeed(f"curl -s -m 30 -o {out} -w '\\n%{{http_code}}' {args} 'http://{VIP}{path}'")
    text, _, code = res.rpartition("\n")
    return int(code), text


def ok(method, path, body=None, token=None):
    code, text = http(method, path, body, token)
    assert code in (200, 204), f"{method} {path} answered {code}: {text}"
    return json.loads(text) if text.strip() else None


def log_in():
    return ok("POST", "/Users/AuthenticateByName", {"Username": ADMIN, "Pw": ADMIN_PW})["AccessToken"]


def user_names(token):
    return {u["Name"] for u in ok("GET", "/Users", token=token)}


def movies(token):
    return ok("GET", "/Items?Recursive=true&IncludeItemTypes=Movie", token=token)["Items"]


def journal(m):
    return m.execute(f"journalctl -t 'expanse-block-default-{NAME}-0' --no-pager -n 200 2>&1")[1]


form("jellyfin")
for m in MACHINES.values():
    wait_agent_ready(m)

with subtest("a manifest with a bad publishedServerUrl is rejected"):
    bad = MANIFEST.replace("http://media.test", "media.test")
    n1.succeed(f"echo {base64.b64encode(bad.encode()).decode()} | base64 -d > /tmp/bad.yaml")
    out = n1.fail(f"expanse ctl block apply {SOCK} -f /tmp/bad.yaml 2>&1")
    assert "publishedServerUrl" in out, f"rejection does not name publishedServerUrl: {out}"

with subtest("deploy a SINGLETON media/jellyfin block behind a VIP"):
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

with subtest("Jellyfin answers, serves its web client and keeps the durable database settings"):
    try:
        client.wait_until_succeeds(f"curl -sf -m 5 http://{VIP}/health | grep -q Healthy", timeout=240)
    except Exception:
        print(journal(MACHINES[holder]))
        raise
    assert "<html" in client.succeed(f"curl -sfL -m 10 http://{VIP}/web/").lower(), "the web client is not served"
    db_xml = MACHINES[holder].succeed("cat /var/lib/expanse/volumes/*/mnt/jellyfin/config/database.xml")
    assert "<Key>syncmode</Key>" in db_xml and "<Value>2</Value>" in db_xml, db_xml

with subtest("the startup wizard creates the administrator"):
    ok("POST", "/Startup/Configuration", {"UICulture": "en-US", "MetadataCountryCode": "US",
                                          "PreferredMetadataLanguage": "en"})
    ok("GET", "/Startup/User")
    ok("POST", "/Startup/User", {"Name": ADMIN, "Password": ADMIN_PW})
    ok("POST", "/Startup/Complete")
    token = log_in()

with subtest("a library on the nodes' media path is scanned and streams"):
    ok("POST", "/Library/VirtualFolders?name=Movies&collectionType=movies&paths=%2Fsrv%2Fmedia%2Fmovies"
               "&refreshLibrary=true", {"LibraryOptions": {}}, token)
    wait_for(lambda: len(movies(token)) == 1, "the library scan finds the movie", timeout=180)
    movie = movies(token)[0]
    size = int(MACHINES[holder].succeed("stat -Lc %s '/srv/media/movies/Test Movie (2024)/Test Movie (2024).mkv'"))
    code, _ = http("GET", f"/Videos/{movie['Id']}/stream?static=true", token=token, out="/tmp/movie")
    streamed = int(client.succeed("stat -c %s /tmp/movie"))
    assert code == 200 and streamed == size, f"streaming answered {code} with {streamed} of {size} bytes"

with subtest("crash the serving node right after users are acknowledged"):
    created = {f"viewer{n}" for n in range(USERS)}
    for name in sorted(created):
        ok("POST", "/Users/New", {"Name": name, "Password": "viewer-password"}, token)
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

with subtest("the survivor has every acknowledged user, the library, and honours the pre-crash token"):
    try:
        client.wait_until_succeeds(f"curl -sf -m 5 http://{VIP}/health | grep -q Healthy", timeout=240)
    except Exception:
        print(journal(MACHINES[new_holder]))
        raise
    print(f"Jellyfin answered again {time.time() - t0:.1f}s after the crash")
    missing = created - user_names(token)
    assert not missing, f"{len(missing)} of {USERS} acknowledged users lost across failover: {sorted(missing)}"
    assert [m["Id"] for m in movies(token)] == [movie["Id"]], "the library lost its movie"
    assert ok("GET", "/System/Info/Public")["StartupWizardCompleted"], "the startup wizard reopened"

with subtest("the survivor still saves changes"):
    ok("POST", "/Users/New", {"Name": "after-failover", "Password": "viewer-password"}, token)
    assert "after-failover" in user_names(token)
    print("MEDIA-JELLYFIN DONE")
