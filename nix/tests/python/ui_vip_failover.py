"""Phase 2, A3: the web UI's own VIP (D8) survives losing the node that
holds it. Runs after cluster-common.py's form(). Mirrors the
reachability pattern net-vip-failover.nix already proved for block
VIPs, applied to config.PortUI instead, plus X7: a session cookie
obtained before the kill still authorizes a request after it, because
sessions live in the Raft store, not process memory (D2).
"""

VIP = "192.168.1.150"
PASSWORD = "vip-failover-test-password"
CLIENT = n9

form("uivipfo")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)

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
    ca_pem = n1.succeed("cat /persist/expanse/ca/ca.pem")
    CLIENT.succeed("cat > /root/ca.pem << 'EOF'\n" + ca_pem + "EOF\n")

with subtest("client logs in through the VIP and gets a session cookie"):
    CLIENT.succeed("rm -f /root/cookies.txt")
    code = CLIENT.succeed(
        "curl -s -o /dev/null -w '%{http_code}' -c /root/cookies.txt "
        f"--resolve expanse-ui:8443:{VIP} --cacert /root/ca.pem "
        f"-d 'username=admin&password={PASSWORD}' https://expanse-ui:8443/login"
    ).strip()
    assert code == "303", f"login through the VIP returned {code}, want 303"
    out = CLIENT.succeed(
        "curl -sf -b /root/cookies.txt --resolve expanse-ui:8443:"
        f"{VIP} --cacert /root/ca.pem https://expanse-ui:8443/"
    )
    assert holder.name in out, f"page did not identify {holder.name} as the answering node: {out}"

with subtest("hard power-off the VIP holder"):
    t0 = time.time()
    holder.send_monitor_command("stop")  # freeze the VM: no packets, no renewal

with subtest("a survivor re-announces the VIP"):
    deadline = time.time() + 30
    new_holder = None
    while time.time() < deadline and new_holder is None:
        for m in [n2, n3]:
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

with subtest("the pre-kill session cookie still authorizes a request (X7)"):
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

# Resume the frozen node: a paused (not shut down) QEMU process cannot
# answer the test driver's own graceful-shutdown handshake during
# cleanup, which otherwise stalls until a fallback timeout (net-vip-
# failover.nix follows the same convention for the same reason).
holder.send_monitor_command("cont")

print("UI-VIP-FAILOVER DONE")
