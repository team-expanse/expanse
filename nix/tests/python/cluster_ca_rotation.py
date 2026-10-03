"""CA rotation on a running 3-node cluster, every daemon up throughout.

Spliced after cluster-common.py (n1..n3, form, kv, wait_quorum, has_node, leader_of, status).
"""

SOCK = "--socket /run/expanse/agent.sock"


def a_follower():
    lead = leader_of(status(n1))
    return next(m for m in (n1, n2, n3) if m.name != lead)


def all_up():
    for m in (n1, n2, n3):
        m.succeed("systemctl is-active expansed.service")


form("ca-rotation")

with subtest("baseline: a write survives, capture the pre-rotation CA trust record"):
    rc, out = kv(n1, "put /rotate/marker before")
    assert rc == 0, out
    orig_trust = n1.succeed(f"expanse ctl kv get /cluster/ca/trust {SOCK}")
    assert '"outgoing"' not in orig_trust, f"already rotating before the test started: {orig_trust}"

with subtest("a follower starts the rotation with its agent running"):
    f = a_follower()
    out = f.succeed(f"expanse cluster ca rotate {SOCK} 2>&1")
    assert "CA rotation started" in out, out
    all_up()

with subtest("ca status reports progress until every node renewed, with no restart"):
    deadline = time.time() + 60
    out = ""
    while time.time() < deadline:
        out = n2.succeed(f"expanse cluster ca status {SOCK}")
        if "safe to run" in out:
            break
        assert "rotating" in out, out
        time.sleep(2)
    assert "safe to run" in out, f"nodes never all renewed within 60s: {out}"
    nodes = n1.succeed(f"expanse ctl kv list /nodes/ {SOCK}")
    fps = re.findall(r'"ca_fp":"([0-9a-f]+)"', nodes)
    assert len(fps) == 3 and len(set(fps)) == 1, f"nodes not on one CA fingerprint: {nodes}"
    all_up()

with subtest("zero downtime throughout renewal: writes/reads still succeed on every node"):
    for i, m in enumerate([n1, n2, n3]):
        rc, out = kv(m, f"put /rotate/post{i} ok")
        assert rc == 0, out
        rc, out = kv(m, f"get /rotate/post{i}")
        assert rc == 0 and out.strip() == "ok", out

with subtest("complete rotation from another node: old CA retired, no node evicted"):
    out = n3.succeed(f"expanse cluster ca complete {SOCK} 2>&1")
    assert "outgoing CA retired" in out, out
    rep = wait_quorum("3/2", 30)
    for nid in ("n1", "n2", "n3"):
        assert has_node(rep, nid), f"{nid} missing after `ca complete`: {rep}"
    new_trust = n1.succeed(f"expanse ctl kv get /cluster/ca/trust {SOCK}")
    assert '"outgoing"' not in new_trust, f"outgoing CA still present after complete: {new_trust}"
    assert new_trust != orig_trust, "CA trust record is unchanged from before the rotation"
    assert "no rotation in progress" in n1.succeed(f"expanse cluster ca status {SOCK}")

with subtest("final sanity: reads/writes across every node after the full rotation"):
    rc, out = kv(n1, "get /rotate/marker")
    assert rc == 0 and out.strip() == "before", out
    rc, out = kv(n2, "put /rotate/final done")
    assert rc == 0, out
    rc, out = kv(n3, "get /rotate/final")
    assert rc == 0 and out.strip() == "done", out
