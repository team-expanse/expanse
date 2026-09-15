# Shared Python helpers for the Phase 05 network VM tests (§6): the
# external client VM (a non-cluster machine that reaches VIPs over the
# LAN) and WireGuard/overlay assertions shared by net-* tests.
#
# Deliberately does NOT shadow cluster-common.py's IP/addr: include both
# in a test that needs the two namespaces. Like block-common.py, this file
# assumes cluster-common.py was spliced first (its `re`/`time` imports are
# reused; re-splicing them triggers F811 lint failures).

# The NixOS test driver assigns deterministic eth1 IPv4s in machine
# order (matches cluster-common.py's table); the external client rides
# the same LAN.
CLIENT_IP = "192.168.1.10"

# Overlay address plan (§3): node at index N holds 10.42.N.1 on exp0.
OVERLAY = {}


def overlay_of(m):
    """A node's overlay IP, resolved once from its exp0 interface."""
    name = m.name
    if name not in OVERLAY:
        rc, out = m.execute("ip -4 -o addr show exp0")
        match = re.search(r"10\.42\.(\d+)\.1", out)
        assert match, f"{name}: no 10.42.N.1 on exp0: {out}"
        OVERLAY[name] = f"10.42.{match.group(1)}.1"
    return OVERLAY[name]


def wg_peers(m):
    """The set of peer public keys configured on the node's exp0."""
    return set(m.succeed("wg show exp0 peers").split())


def wait_wg_peers(m, count, timeout=60):
    """Poll until exp0 carries exactly `count` peers."""
    deadline = time.time() + timeout
    out = ""
    while time.time() < deadline:
        rc, out = m.execute("wg show exp0 peers 2>/dev/null || true")
        peers = set(out.split())
        if len(peers) == count:
            return peers
        time.sleep(1)
    raise AssertionError(f"{m.name}: exp0 never reached {count} peers: {out}")


# Max ICMP data size through exp0: MTU 1420 − 28 (IP + ICMP headers).
DF_SIZE = 1392


def ping_matrix(machines, size=None):
    """Full pairwise overlay ping. size=None → default; ("df", n) →
    n-byte don't-fragment ping (the §4.1 MTU diagnostic)."""
    for src in machines:
        for dst in machines:
            if src is dst:
                continue
            if isinstance(size, tuple) and size[0] == "df":
                src.succeed(
                    f"ping -c 1 -W 3 -M do -s {size[1]} {overlay_of(dst)}"
                )
            else:
                src.succeed(f"ping -c 1 -W 3 {overlay_of(dst)}")
