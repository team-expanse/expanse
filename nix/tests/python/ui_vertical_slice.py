"""Phase 2, E1: the phase-closing vertical slice (X1, X2, X7) -- this
phase's equivalent of Phase 1's E2 durability loop. Deploy nginx from
the UI through the UI's own VIP (D8), then hard-kill the node holding
that VIP while the deploy is still in flight. Both halves of X2 must
hold: the interface stays reachable (a survivor re-announces the VIP)
and the deployment still completes, observed through the new holder,
not the dead one. The pre-kill session cookie must still authorize
requests post-failover (X7), exercised end-to-end here rather than in
isolation as A3's own ui_vip_failover.py did.

`base64`, `wait_phase`, `get_json`, `placement_nodes` come from
block-common.py, spliced in before this file.
"""

VIP = "192.168.1.150"
PASSWORD = "e1-vertical-slice-password"
CLIENT = n9

# Same manifest ui_blocks.py's own X1 deploy uses -- already proven.
NGINX_YAML = (
    "apiVersion: expanse.io/v1\nkind: Block\n"
    "metadata:\n  name: web\n  namespace: default\n"
    "spec:\n  type: web/nginx\n  replicas: 1\n"
    "  placement:\n    antiAffinity: ANTI_AFFINITY_NODE\n"
    "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
    "  config:\n    port: 8080\n    serverName: web\n"
)


def ui_login(m, password=PASSWORD, jar="/root/cookies.txt", host="expanse-ui", addr=VIP):
    """Log in through host resolved to addr (the VIP -- a successful
    login already proves the interface is reachable through it);
    returns the CSRF token."""
    m.succeed(f"rm -f {jar}")
    code = m.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' -c {jar} "
        f"--resolve {host}:8443:{addr} --cacert /root/ca.pem "
        f"-d 'username=admin&password={password}' https://{host}:8443/login"
    ).strip()
    assert code == "303", f"login through the VIP returned {code}, want 303"
    csrf = m.succeed(f"awk -F'\\t' '$6==\"expanse_csrf\"{{print $7}}' {jar}").strip()
    assert csrf, f"no expanse_csrf cookie in {jar}"
    return csrf


form("e1vslice")
for m in [n1, n2, n3]:
    wait_agent_ready(m)

with subtest("set a known admin password"):
    n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")

with subtest("exactly one node holds the UI VIP"):
    deadline = time.time() + 30
    holder = None
    while time.time() < deadline and holder is None:
        for m in [n1, n2, n3]:
            rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {VIP} || true")
            if rc == 0 and out.strip():
                holder = m
                break
        time.sleep(2)
    assert holder is not None, "no node holds the UI VIP"

with subtest("provision the cluster CA onto the external client"):
    ca_pem = n1.succeed("cat /persist/expanse/ca/ui-ca.pem")
    CLIENT.succeed("cat > /root/ca.pem << 'EOF'\n" + ca_pem + "EOF\n")

with subtest("client logs in through the VIP (X1's interface, X3's auth gate)"):
    csrf = ui_login(CLIENT)

with subtest("deploy nginx via the UI through the VIP, then immediately kill the holder"):
    manifest_b64 = base64.b64encode(NGINX_YAML.encode()).decode()
    CLIENT.succeed(f"echo {manifest_b64} | base64 -d > /root/manifest.yaml")
    out = CLIENT.succeed(
        "curl -s -o /dev/null -D /root/deploy-headers.txt -b /root/cookies.txt "
        f"-H 'X-CSRF-Token: {csrf}' --resolve expanse-ui:8443:{VIP} --cacert /root/ca.pem "
        "--data-urlencode manifest@/root/manifest.yaml https://expanse-ui:8443/blocks; "
        "cat /root/deploy-headers.txt"
    )
    assert "303" in out.splitlines()[0], f"deploy did not redirect: {out}"
    t0 = time.time()
    holder.send_monitor_command("stop")  # freeze mid-deploy: no packets, no renewal

with subtest("a survivor re-announces the VIP (X2, first half)"):
    deadline = time.time() + 30
    new_holder = None
    while time.time() < deadline and new_holder is None:
        for m in [n1, n2, n3]:
            if m.name == holder.name:
                continue
            rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {VIP} || true")
            if rc == 0 and out.strip():
                new_holder = m
                break
        time.sleep(1)
    assert new_holder is not None, "no survivor took over the UI VIP"
    assert new_holder.name != holder.name
    print(f"UI VIP re-announced on {new_holder.name} after {time.time() - t0:.1f}s")

with subtest("the pre-kill session cookie still authorizes a request through the new holder (X7)"):
    deadline = time.time() + 30
    out = ""
    while time.time() < deadline:
        rc, out = CLIENT.execute(
            "curl -sf -b /root/cookies.txt --resolve expanse-ui:8443:"
            f"{VIP} --cacert /root/ca.pem https://expanse-ui:8443/ || true"
        )
        if rc == 0 and new_holder.name in out:
            break
        time.sleep(1)
    assert new_holder.name in out, (
        f"post-failover request via the pre-kill cookie did not reach {new_holder.name}: {out}"
    )

with subtest("the deployment completes, observed through a survivor, not the dead node (X2, second half)"):
    # 90s: covers the §4.4 unreachable grace (30s default) plus a
    # reschedule pass, in case the replica had landed on the killed
    # holder before it was frozen.
    survivor = next(m for m in [n1, n2, n3] if m.name != holder.name)
    b = wait_phase(survivor, "web", ["RUNNING"], 90)
    nodes = placement_nodes(b)
    assert nodes, f"no live placement after failover: {b}"
    assert holder.name not in nodes, f"a live placement is still on the dead node: {nodes}"

with subtest("nginx actually answers on its (surviving) node"):
    host = next(iter(placement_nodes(b)))
    m = {"n1": n1, "n2": n2, "n3": n3}[host]
    code = m.succeed(
        "curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://localhost:8080/"
    ).strip()
    assert code and code != "000", f"nginx on {host}:8080 did not respond (code={code!r})"

# Resume the frozen node: a paused (not shut down) QEMU process cannot
# answer the test driver's own graceful-shutdown handshake during
# cleanup, which otherwise stalls until a fallback timeout (ui_vip_
# failover.py follows the same convention for the same reason).
holder.send_monitor_command("cont")

print("UI-VERTICAL-SLICE DONE")
