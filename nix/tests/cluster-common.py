# Shared Python helpers for the Phase 03 cluster VM tests (§6).
# These are spliced into each test's testScript via readFile; the
# machines n1/n2/n3 come from the driver context.
#
# Enrollment order matters: the CLI owns the bolt lock + raft port
# while it runs, and the join endpoint (:7446) is served by n1's
# daemon. So: stop daemons → init → mint token → start n1's daemon →
# join n2 → start n2's daemon → join n3 → start n3's daemon → assert.
import re
import time

# The NixOS test driver assigns deterministic eth1 IPv4s in machine
# order. Raft binds 0.0.0.0 (IPv4), so advertise the v4 addr — node
# NAMEs resolve to the driver's IPv6 addrs, which the v4-only raft
# transport can never dial.
IP = {"n1": "192.168.1.1", "n2": "192.168.1.2", "n3": "192.168.1.3", "n4": "192.168.1.4"}


def addr(m):
    return IP[m.name]



def form(name="test", role3="voter", uses=3):
    """Bootstrap the 3-node cluster and wait for full quorum. `uses`
    is the join-token budget; a higher value leaves joins for later
    (the token persists at /root/join-token on each node)."""
    start_all()
    for m in [n1, n2, n3]:
        m.wait_for_unit("multi-user.target")
        m.succeed("systemctl stop expansed.service")

    n1.succeed(
        "expanse cluster init --data-dir /persist/expanse "
        f"--name {name} --node-id n1 --advertise-addr 192.168.1.1:7444 --expect 3"
    )
    token = ""
    for _ in range(30):
        rc, out = n1.execute(
            f"expanse cluster token --data-dir /persist/expanse create --uses {uses} 2>/dev/null || true"
        )
        match = re.search(r"expanse-join-[A-Za-z0-9_-]+", out)
        if match:
            token = match.group(0)
            break
        time.sleep(1)
    assert token, "no join token minted"

    n1.succeed("systemctl start expansed.service")
    n1.wait_for_unit("expansed.service")

    join_and_start(n2, token, "voter")
    join_and_start(n3, token, role3)
    wait_quorum("3/2", 60)


def join_and_start(m, token, role="voter"):
    """Join a node (with retry — the leader may be mid-election) and
    start its daemon immediately so the cluster doesn't lose quorum
    waiting for the next joiner."""
    m.succeed(f"echo -n '{token}' > /root/join-token")
    deadline = time.time() + 60
    while True:
        rc, out = m.execute(
            "expanse cluster join --data-dir /persist/expanse "
            "--node-id $(hostname) --address 192.168.1.1:7446 "
            "--token $(cat /root/join-token) "
            f"--advertise-addr {addr(m)}:7444 --role {role} 2>&1"
        )
        if rc == 0:
            break
        if "already" in out or "conflict" in out.lower():
            break  # idempotent re-join
        if time.time() > deadline:
            raise AssertionError(f"{m.name} join failed: {out}")
        time.sleep(2)
    print(f"{m.name} joined as {role}")
    m.succeed("systemctl start expansed.service")
    m.wait_for_unit("expansed.service")
    wait_agent_ready(m)


def wait_agent_ready(m):
    for _ in range(60):
        rc, _ = m.execute("test -S /run/expanse/agent.sock")
        if rc == 0:
            return
        time.sleep(1)
    raise AssertionError(f"{m.name} agent socket never appeared")


def status(m):
    rc, out = m.execute(
        "expanse cluster status --socket /run/expanse/agent.sock 2>/dev/null || true"
    )
    return out


def wait_quorum(want, timeout):
    """Poll until n1 sees the wanted quorum (e.g. '3/3')."""
    deadline = time.time() + timeout
    rep = ""
    while time.time() < deadline:
        rep = status(n1)
        if f"quorum:    {want}" in rep:
            return rep
        time.sleep(1)
    raise AssertionError(f"no quorum {want} within {timeout}s: {rep}")


def leader_of(rep):
    ls = leaders(rep)
    return ls[0] if ls else ""


def kv(m, args):
    """Run an `expanse ctl kv` command via the node's agent socket."""
    return m.execute(f"expanse ctl kv --socket /run/expanse/agent.sock {args} 2>&1")


def leaders(rep):
    """IDs of nodes whose STATE column says 'leader'."""
    out = []
    for ln in rep.splitlines():
        parts = ln.split()
        if len(parts) >= 4 and parts[0] in ("n1", "n2", "n3") and parts[2] == "leader":
            out.append(parts[0])
    return out


def has_node(rep, node_id):
    """True when the report's table has a row for node_id."""
    for ln in rep.splitlines():
        parts = ln.split()
        if parts and parts[0] == node_id and len(parts) >= 4:
            return True
    return False
