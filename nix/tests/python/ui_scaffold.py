"""Phase 2, A1: the web UI's TLS listener comes up on every cluster
node (config.PortUI, cert signed by the cluster CA) and serves the
placeholder page. Runs after cluster-common.py's form().

Node certs carry no IP SANs (only the node ID as a DNSName), so every
curl below resolves the node's own name to its loopback address rather
than connecting by IP -- matching how a real client would reach it
(by name/VIP, not a bare address a cert was never issued for).

A2 gated every page but /static behind auth after this test was first
written (X3: an unauthenticated request is redirected to /login, never
served the page or told whether a path exists) -- the placeholder-page
and unknown-path subtests below log in first, matching that design
rather than the pre-auth behavior this test originally asserted.
"""

CAFILE = "/persist/expanse/ca/ui-ca.pem"
PASSWORD = "ui-scaffold-test-password"

form("test")

with subtest("ctl admin reset-password sets a known password on every node"):
    for m in [n1, n2, n3]:
        m.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")

with subtest("every node serves the UI placeholder over TLS signed by the cluster CA"):
    for m in [n1, n2, n3]:
        jar = f"/tmp/ui-cookies-{m.name}.txt"
        code = m.succeed(
            f"curl -s -o /dev/null -w '%{{http_code}}' -c {jar} "
            f"--resolve {m.name}:8443:127.0.0.1 --cacert {CAFILE} "
            f"-d 'username=admin&password={PASSWORD}' https://{m.name}:8443/login"
        ).strip()
        assert code == "303", f"{m.name}: login returned {code}, want 303"
        out = m.succeed(
            f"curl -sf -b {jar} --resolve {m.name}:8443:127.0.0.1 --cacert {CAFILE} "
            f"https://{m.name}:8443/"
        )
        assert m.name in out, f"{m.name}: node ID missing from placeholder page: {out}"
        assert "htmx" in out, f"{m.name}: htmx script tag missing: {out}"

with subtest("static assets (htmx, sse extension, stylesheet) are served without auth"):
    for path in ["htmx.min.js", "htmx-sse.min.js", "style.css"]:
        size = n1.succeed(
            f"curl -sf --resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
            f"https://n1:8443/static/{path} | wc -c"
        ).strip()
        assert int(size) > 0, f"{path} served empty"

with subtest("an unauthenticated request never reveals routing (X3)"):
    code = n1.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} https://n1:8443/does-not-exist"
    ).strip()
    assert code == "303", f"unauthenticated unknown path returned {code}, want 303 (redirect to /login)"

with subtest("an authenticated unknown path is a plain 404, not a crash"):
    code = n1.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' -b /tmp/ui-cookies-n1.txt "
        f"--resolve n1:8443:127.0.0.1 --cacert {CAFILE} https://n1:8443/does-not-exist"
    ).strip()
    assert code == "404", f"authenticated unknown path returned {code}, want 404"

with subtest("TLS is real, not decorative: a client that does not trust the cluster CA is refused"):
    n1.fail("curl -sf --resolve n1:8443:127.0.0.1 https://n1:8443/")

print("UI-SCAFFOLD DONE")
