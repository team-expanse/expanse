"""Phase 2, B1: the cluster overview page over the web UI -- nodes,
quorum, leader and generation, pushed live over SSE (X4), never
polled. Builds on ui_auth.py's login flow and cluster-common.py's own
form()/status() helpers as an independent cross-check.
"""

# base64 comes from block-common.py in ui_blocks.py, not spliced here --
# this test needs no manifest encoding, so no extra import is needed.

CAFILE = "/persist/expanse/ca/ui-ca.pem"
PASSWORD = "ui-cluster-test-password"


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


form("uicluster")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)

with subtest("set a known admin password and log in to n1's UI"):
    n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")
    ui_login(n1)

with subtest("the overview page shows all three nodes and quorum (X4)"):
    body = n1.succeed(
        "curl -sf -b /tmp/ui-cookies.txt --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} https://n1:8443/cluster"
    )
    for nid in ("n1", "n2", "n3"):
        assert nid in body, f"overview missing node {nid}: {body}"
    assert "3/2" in body, f"overview missing quorum 3/2: {body}"

with subtest("open the live SSE stream in the background"):
    n1.execute(
        "nohup curl -s -N -b /tmp/ui-cookies.txt --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} https://n1:8443/cluster/events "
        "> /tmp/cluster-sse.log 2>&1 & echo started"
    )
    # Let the connection land and deliver its initial snapshot before the
    # next subtest starts mutating state -- otherwise a very early write
    # could race the client's own TCP handshake, not this server's watch
    # (the server-side watch/snapshot race itself is fixed: the watch
    # starts before the snapshot read, so no write after this point can
    # ever be silently dropped, only the connection setup can be slow).
    time.sleep(3)
    snap1 = n1.succeed("cat /tmp/cluster-sse.log")
    assert "event: cluster" in snap1, f"no initial SSE snapshot: {snap1!r}"

with subtest("failing a node is reflected live, no manual refresh (X4)"):
    n3.succeed("systemctl stop expansed.service")
    # DefaultUnreachableAfter (15s) + DefaultInterval (5s) + margin.
    deadline = time.time() + 45
    out = ""
    while time.time() < deadline:
        out = n1.succeed("cat /tmp/cluster-sse.log")
        if "unreachable" in out.lower():
            break
        time.sleep(2)
    assert "unreachable" in out.lower(), f"SSE stream never reflected n3 going unreachable: {out!r}"

with subtest("a generation change is reflected live, no manual refresh (X4)"):
    before = n1.succeed("cat /tmp/cluster-sse.log")
    gens_before = re.findall(r"Generation</div><div class=\"kv-value\">(?:<a [^>]*>)?(\d+)", before)
    gen_before = int(gens_before[-1]) if gens_before else 0
    n1.succeed("expanse ctl kv put /cluster/config/uitest ui-value --socket /run/expanse/agent.sock")
    deadline = time.time() + 20
    gen_after = gen_before
    out = before
    while time.time() < deadline:
        out = n1.succeed("cat /tmp/cluster-sse.log")
        gens = re.findall(r"Generation</div><div class=\"kv-value\">(?:<a [^>]*>)?(\d+)", out)
        gen_after = int(gens[-1]) if gens else gen_before
        if gen_after > gen_before:
            break
        time.sleep(1)
    assert gen_after > gen_before, (
        f"SSE stream never reflected the generation bump ({gen_before} -> ?): {out!r}"
    )

n1.execute("pkill -f 'cluster/events' || true")
print("UI-CLUSTER DONE")
