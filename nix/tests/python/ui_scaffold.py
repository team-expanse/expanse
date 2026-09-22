"""Phase 2, A1: the web UI's TLS listener comes up on every cluster
node (config.PortUI, cert signed by the cluster CA) and serves the
placeholder page. Runs after cluster-common.py's form().

Node certs carry no IP SANs (only the node ID as a DNSName), so every
curl below resolves the node's own name to its loopback address rather
than connecting by IP -- matching how a real client would reach it
(by name/VIP, not a bare address a cert was never issued for).
"""

CAFILE = "/persist/expanse/ca/ca.pem"

form("test")

with subtest("every node serves the UI placeholder over TLS signed by the cluster CA"):
    for m in [n1, n2, n3]:
        out = m.succeed(
            f"curl -sf --resolve {m.name}:8443:127.0.0.1 --cacert {CAFILE} "
            f"https://{m.name}:8443/"
        )
        assert m.name in out, f"{m.name}: node ID missing from placeholder page: {out}"
        assert "htmx" in out, f"{m.name}: htmx script tag missing: {out}"

with subtest("static assets (htmx, sse extension, stylesheet) are served"):
    for path in ["htmx.min.js", "htmx-sse.min.js", "style.css"]:
        size = n1.succeed(
            f"curl -sf --resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
            f"https://n1:8443/static/{path} | wc -c"
        ).strip()
        assert int(size) > 0, f"{path} served empty"

with subtest("an unknown path is a plain 404, not a crash"):
    code = n1.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} https://n1:8443/does-not-exist"
    ).strip()
    assert code == "404", f"unknown path returned {code}, want 404"

with subtest("TLS is real, not decorative: a client that does not trust the cluster CA is refused"):
    n1.fail("curl -sf --resolve n1:8443:127.0.0.1 https://n1:8443/")

print("UI-SCAFFOLD DONE")
