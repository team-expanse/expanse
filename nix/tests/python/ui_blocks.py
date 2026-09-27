"""Phase 2, C1: block handlers over the web UI. Runs after
cluster-common.py's form() and reuses block-common.py's CLI-side
assertions (get_json/wait_phase/placement_nodes) as an independent
cross-check that what the UI wrote is exactly what the control plane
sees -- the same "two readers of one truth" pattern ui_auth.py used for
its own no-plaintext-leak check.

Deploys web/nginx (X1's literal wording), not util/echo: the manifest
below is proven already by net-vip-basic.nix's own nginx deploy.
"""

# base64 comes from block-common.py, spliced in before this file.

CAFILE = "/persist/expanse/ca/ui-ca.pem"
PASSWORD = "ui-blocks-test-password"

NGINX_YAML = (
    "apiVersion: expanse.io/v1\nkind: Block\n"
    "metadata:\n  name: web\n  namespace: default\n"
    "spec:\n  type: web/nginx\n  replicas: 1\n"
    "  placement:\n    antiAffinity: ANTI_AFFINITY_NODE\n"
    "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
    "  config:\n    port: 8080\n    serverName: web\n"
)


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


form("uiblocks")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)

with subtest("set a known admin password"):
    n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")

with subtest("log in to n1's UI"):
    csrf = ui_login(n1)

with subtest("deploy web/nginx via the UI (X1)"):
    manifest_b64 = base64.b64encode(NGINX_YAML.encode()).decode()
    n1.succeed(f"echo {manifest_b64} | base64 -d > /tmp/manifest.yaml")
    out = n1.succeed(
        "curl -s -o /dev/null -D /tmp/deploy-headers.txt -b /tmp/ui-cookies.txt "
        f"-H 'X-CSRF-Token: {csrf}' --resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
        "--data-urlencode manifest@/tmp/manifest.yaml https://n1:8443/blocks; "
        "cat /tmp/deploy-headers.txt"
    )
    assert "303" in out.splitlines()[0], f"deploy did not redirect: {out}"
    assert "location: /blocks/default/web" in out.lower(), f"unexpected redirect target: {out}"

with subtest("the block reaches RUNNING, observed live over SSE (X5)"):
    # 30s, matching block-deploy.nix's own G4.5 budget (controllerPeriod
    # is tightened to 5s above so promotion is prompt, not lucky).
    # Check the captured text directly, not the pipeline's exit code:
    # grep -m1 quitting early makes curl's own exit racy under pipefail
    # even once a real match already printed.
    _, out = n1.execute(
        "timeout 30 curl -s -N -b /tmp/ui-cookies.txt --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} https://n1:8443/blocks/default/web/events | grep RUNNING || true"
    )
    assert "RUNNING" in out, f"SSE stream never reported RUNNING within 30s: {out!r}"

with subtest("the CLI's own view agrees with what the UI wrote"):
    b = wait_phase(n1, "web", ["RUNNING"], 10)
    assert b["spec"]["type"] == "web/nginx"
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"want 1 placement, got {nodes}"
    host = replica_node(b, 0)
    m = {"n1": n1, "n2": n2, "n3": n3}[host]

with subtest("nginx actually answers on its node"):
    code = m.succeed(
        "curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://localhost:8080/"
    ).strip()
    assert code and code != "000", f"nginx on {host}:8080 did not respond (code={code!r})"

with subtest("scale to 2 replicas via the UI"):
    body = n1.succeed(
        "curl -s -b /tmp/ui-cookies.txt "
        f"-H 'X-CSRF-Token: {csrf}' --resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
        "-d 'replicas=2' https://n1:8443/blocks/default/web/scale"
    )
    assert "/2" in body, f"scaled fragment missing the new count: {body}"
    # Poll placement count directly, not wait_phase(["RUNNING"]): the
    # block's phase is still RUNNING from before the scale (nothing
    # demotes it the instant replicas changes), so waiting on phase alone
    # would return immediately on that stale 1-replica snapshot.
    deadline = time.time() + 30
    b, nodes = None, set()
    while time.time() < deadline:
        b = get_json(n1, "web")
        nodes = placement_nodes(b) if b else set()
        if len(nodes) == 2:
            break
        time.sleep(2)
    assert len(nodes) == 2, f"want 2 distinct nodes after scale, got {nodes}: {(b or {}).get('status')}"
    wait_phase(n1, "web", ["RUNNING"], 30)

with subtest("tail live logs via SSE (X5), on the node actually hosting replica 0"):
    csrf_host = ui_login(m, jar="/tmp/ui-cookies-host.txt")
    rc, out = m.execute(
        "timeout 15 curl -s -N -b /tmp/ui-cookies-host.txt "
        f"--resolve {host}:8443:127.0.0.1 --cacert {CAFILE} "
        f"'https://{host}:8443/blocks/default/web/logs/stream?replica=0'"
    )
    assert "event: line" in out, f"log SSE stream produced no lines on {host}: {out!r}"

with subtest("delete via the UI"):
    n1.succeed(
        "curl -sf -o /dev/null -b /tmp/ui-cookies.txt "
        f"-H 'X-CSRF-Token: {csrf}' --resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
        "-d '' https://n1:8443/blocks/default/web/delete"
    )
    code = n1.succeed(
        "curl -s -o /dev/null -w '%{http_code}' -b /tmp/ui-cookies.txt "
        f"--resolve n1:8443:127.0.0.1 --cacert {CAFILE} https://n1:8443/blocks/default/web"
    ).strip()
    assert code == "404", f"block still reachable after UI delete: {code}"

print("UI-BLOCKS DONE")
