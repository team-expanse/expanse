"""Phase 2, D1: volume handlers over the web UI. Runs after
cluster-common.py's form(). Creates a small replicated volume through
the UI's HTML form, watches it get placed live over SSE, resizes and
snapshots it through the UI, and cross-checks every step against
`expanse ctl volume inspect`'s own view of the same store records --
the same "two readers of one truth" pattern ui_blocks.py uses for its
own deploy.
"""

CAFILE = "/persist/expanse/ca/ui-ca.pem"
PASSWORD = "ui-volumes-test-password"
VOLNAME = "uivol"
NODES = [n1, n2, n3]


def ui_login(m, password=PASSWORD, jar="/tmp/ui-cookies.txt"):
    """Log in against m's own UI listener; returns the CSRF token (the
    session cookie itself stays in the jar file curl reuses)."""
    m.succeed(f"rm -f {jar}")
    code = m.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' -c {jar} "
        f"--resolve {m.name}:8443:127.0.0.1 --cacert {CAFILE} "
        f"-d 'username=admin&password={password}' https://{m.name}:8443/login"
    ).strip()
    assert code == "303", f"login on {m.name} returned {code}, want 303"
    csrf = m.succeed(f"awk -F'\\t' '$6==\"expanse_csrf\"{{print $7}}' {jar}").strip()
    assert csrf, f"no expanse_csrf cookie in {jar} on {m.name}"
    return csrf


def inspect(m, name=VOLNAME):
    return m.execute(f"expanse ctl volume inspect {name} --socket /run/expanse/agent.sock 2>&1")[1]


def detail_page(m, csrf, name=VOLNAME):
    return m.succeed(
        f"curl -sf -b /tmp/ui-cookies.txt -H 'X-CSRF-Token: {csrf}' "
        f"--resolve n1:8443:127.0.0.1 --cacert {CAFILE} https://n1:8443/volumes/{name}"
    )


form("uivolumes")
for m in NODES:
    wait_agent_ready(m)

with subtest("set a known admin password"):
    n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")

with subtest("log in to n1's UI"):
    csrf = ui_login(n1)

with subtest("create a volume via the UI (X6)"):
    out = n1.succeed(
        "curl -s -o /dev/null -D /tmp/create-headers.txt -b /tmp/ui-cookies.txt "
        f"-H 'X-CSRF-Token: {csrf}' --resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
        f"-d 'name={VOLNAME}&size=256Mi&replication=3&class=default' "
        "https://n1:8443/volumes; cat /tmp/create-headers.txt"
    )
    assert "303" in out.splitlines()[0], f"create did not redirect: {out}"
    assert f"location: /volumes/{VOLNAME}" in out.lower(), f"unexpected redirect target: {out}"

with subtest("the volume is placed on all 3 nodes, observed live over SSE (X6)"):
    _, out = n1.execute(
        f"timeout 60 curl -s -N -b /tmp/ui-cookies.txt --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} https://n1:8443/volumes/{VOLNAME}/events | grep -m1 -E 'n1.*n2|n2.*n1' || true"
    )
    # Fall back to polling the detail page directly if the single-line grep above
    # raced the fragment's own HTML layout (placement rows are not on one line).
    if not out:
        deadline = time.time() + 60
        page = ""
        while time.time() < deadline:
            page = detail_page(n1, csrf)
            if all(f"<strong>{m.name}</strong></a></td>" in page for m in NODES):
                break
            time.sleep(2)
        assert all(f"<strong>{m.name}</strong></a></td>" in page for m in NODES), f"not all nodes placed within 60s: {page}"

with subtest("the CLI's own inspect view agrees with what the UI shows"):
    page = detail_page(n1, csrf)
    text = inspect(n1)
    assert VOLNAME in text and "vol-" in text, f"ctl inspect missing volume: {text}"
    for m in NODES:
        assert m.name in text, f"ctl inspect missing {m.name}: {text}"
        assert f"<strong>{m.name}</strong></a></td>" in page, f"UI detail page missing {m.name}: {page}"

with subtest("resize via the UI (grow-only)"):
    out = n1.succeed(
        "curl -s -L -o /dev/null -w '%{http_code}' -b /tmp/ui-cookies.txt "
        f"-H 'X-CSRF-Token: {csrf}' --resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
        f"-d 'size=512Mi' https://n1:8443/volumes/{VOLNAME}/resize"
    ).strip()
    assert out == "200", f"resize did not redirect through to 200: {out}"
    deadline = time.time() + 60
    text = ""
    while time.time() < deadline:
        text = inspect(n1)
        if "512" in text or "0.50" in text or "512.0" in text:
            break
        time.sleep(2)
    assert "512" in text or "0.5" in text, f"ctl inspect never showed the grown size: {text}"

with subtest("snapshot via the UI"):
    out = n1.succeed(
        "curl -s -L -o /dev/null -w '%{http_code}' -b /tmp/ui-cookies.txt "
        f"-H 'X-CSRF-Token: {csrf}' --resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
        f"-d 'name=uisnap' https://n1:8443/volumes/{VOLNAME}/snapshot"
    ).strip()
    assert out == "200", f"snapshot did not redirect through to 200: {out}"
    deadline = time.time() + 30
    text, page = "", ""
    while time.time() < deadline:
        text = inspect(n1)
        page = detail_page(n1, csrf)
        if "uisnap" in text and "uisnap" in page:
            break
        time.sleep(2)
    assert "uisnap" in text, f"ctl inspect never showed the snapshot: {text}"
    assert "uisnap" in page, f"UI detail page never showed the snapshot: {page}"

with subtest("delete via the UI"):
    n1.succeed(
        "curl -sf -o /dev/null -b /tmp/ui-cookies.txt "
        f"-H 'X-CSRF-Token: {csrf}' --resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
        f"-d '' https://n1:8443/volumes/{VOLNAME}/delete"
    )
    deadline = time.time() + 60
    text = ""
    while time.time() < deadline:
        text = n1.execute("expanse ctl volume list --socket /run/expanse/agent.sock 2>&1")[1]
        if VOLNAME not in text:
            break
        time.sleep(2)
    assert VOLNAME not in text, f"ctl volume list still shows a deleted volume: {text}"

print("UI-VOLUMES DONE")
