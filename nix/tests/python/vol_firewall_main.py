"""vol-firewall: with the agent's nftables ruleset enforced and the NixOS firewall off, a volume
replicates over the mesh and its DRBD port is closed to every other interface.

Runs after cluster-common.py and vol_cluster.py; n9 is an off-mesh client on eth1.
"""

SIZE_MIB = 16
DROPPED = 124  # exit status of `timeout` when a SYN gets no answer
REFUSED = 1  # a connect answered with a reset: the packet reached the node


def mesh_endpoint(m, res):
    """The (address, port) DRBD listens on for m's replica, from its resource file."""
    conf = m.succeed(f"cat /etc/drbd.d/{res}.res")
    found = re.search(rf"on {m.name} \{{[^}}]*address (?:ipv4 )?([0-9.]+):(\d+)", conf)
    assert found, f"no address for {m.name} in {conf}"
    return found.group(1), int(found.group(2))


def probe_from_client(addr, port):
    """Exit status of a TCP connect to the mesh address, sent out of eth1 rather than the mesh."""
    n9.succeed(f"ip route replace {addr}/32 dev eth1")
    return n9.execute(f"timeout 3 bash -c '</dev/tcp/{addr}/{port}'")[0]


def open_eth1(m, port):
    """Let eth1 reach port on m; returns the rule's handle so it can be removed."""
    out = m.succeed(f"nft -e -a insert rule inet expanse input iifname eth1 tcp dport {port} accept")
    found = re.search(r"# handle (\d+)", out)
    assert found, f"nft printed no handle: {out}"
    return found.group(1)


form("volfw")

with subtest("the agent's ruleset is enforced and admits DRBD on the mesh interface only"):
    for m in NODES:
        m.wait_until_succeeds(
            "nft list chain inet expanse input | grep -Eq 'iifname .exp0. tcp dport 9500-10499 accept'",
            timeout=120,
        )
        assert "policy drop" in m.succeed("nft list chain inet expanse input")
        m.succeed("! systemctl is-active nftables.service")

with subtest("a volume replicates over the mesh with the firewall enforced"):
    n1.succeed(f"expanse ctl volume create fwvol --size {SIZE_MIB * 2}Mi --replication 3")
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")
    wait_for(lambda: len(primaries(res)) == 1, "one primary")

with subtest("what the primary writes reaches every replica"):
    primary = primaries(res)[0]
    primary.succeed(f"dd if=/dev/urandom of=/tmp/pad bs=1M count={SIZE_MIB}")
    ref = primary.succeed("sha256sum /tmp/pad | cut -d' ' -f1").strip()
    primary.succeed(f"dd if=/tmp/pad of={device_of(primary)} bs=1M oflag=direct conv=fsync")
    wait_for(lambda: all(checksum(m, f"/dev/vg0/{res}", SIZE_MIB) == ref for m in NODES), "the write on every replica")

with subtest("the DRBD port is closed to a client on another interface"):
    for m in NODES:
        addr, port = mesh_endpoint(m, res)
        assert probe_from_client(addr, port) == DROPPED, f"{m.name}: {addr}:{port} answered off-mesh"

with subtest("control: the probe reaches the node, and only the ruleset keeps it from answering"):
    addr, port = mesh_endpoint(n1, res)
    handle = open_eth1(n1, port)
    # DRBD listens only while a connection is being made, so an open path ends in a reset.
    assert probe_from_client(addr, port) == REFUSED, "the probe does not reach the node even with eth1 opened"
    n1.succeed(f"nft delete rule inet expanse input handle {handle}")
    assert probe_from_client(addr, port) == DROPPED, "the port is reachable again after the rule is removed"

with subtest("replication is undisturbed by the probes"):
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")
    assert len(primaries(res)) == 1, f"primaries: {[m.name for m in primaries(res)]}"
