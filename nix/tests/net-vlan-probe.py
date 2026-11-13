# Probe body for net-vlan-probe.nix (Phase 17 N0). Observations only.


def lease(ns, dev):
    """Runs one DHCP exchange on dev inside netns ns and returns (exit code, IPv4 addresses)."""
    # ip netns exec gives a private mount namespace; a tmpfs over /run/dhcpcd keeps this client off
    # the host dhcpcd's control socket.
    rc, out = n1.execute(
        f"ip netns exec {ns} sh -c 'mount -t tmpfs none /run/dhcpcd && "
        f"dhcpcd -1 -4 -p -w -t 30 -L -A --nohook resolv.conf --nohook hostname {dev}' 2>&1"
    )
    print(f"dhcpcd {ns}/{dev} exit={rc}\n{out}")
    addrs = n1.succeed(f"ip netns exec {ns} ip -4 -o addr show dev {dev}")
    print(addrs)
    return rc, addrs


def macvlan_ns(ns, parent, dev, mac):
    """Creates netns ns holding a bridge-mode macvlan child of parent with a pinned MAC."""
    n1.succeed(
        f"ip netns add {ns} && "
        f"ip link add link {parent} name {dev} type macvlan mode bridge && "
        f"ip link set {dev} address {mac} netns {ns} && "
        f"ip netns exec {ns} ip link set lo up && "
        f"ip netns exec {ns} ip link set {dev} up"
    )


start_all()
n1.wait_for_unit("multi-user.target")
n2.wait_for_unit("dnsmasq.service")
n1.succeed("mkdir -p /var/lib/dhcpcd /run/dhcpcd")
n2.succeed("tcpdump -i eth1 -e -n -l vlan > /tmp/tagged.txt 2>&1 &")
n2.wait_until_succeeds("grep -q listening /tmp/tagged.txt")

print("--- 0: tagged frames cross the test network: host eth2.99 pings n2's eth1.99 ---")
n2.succeed("ip link add link eth1 name eth1.99 type vlan id 99 && ip addr add 10.99.0.1/24 dev eth1.99 && ip link set eth1.99 up")
n1.succeed("ip link add link eth2 name eth2.99 type vlan id 99 && ip addr add 10.99.0.2/24 dev eth2.99 && ip link set eth2.99 up")
print(n1.execute("ping -c2 -W2 10.99.0.1")[1])
print(n2.succeed("grep -c 'vlan 99' /tmp/tagged.txt || true"))

print("--- 1: trunk child on eth2 (a VM stand-in), before the host has eth2.20 ---")
macvlan_ns("tr1", "eth2", "mvtr", "52:54:00:17:00:01")
n1.succeed(
    "ip netns exec tr1 ip link add link mvtr name mvtr.20 type vlan id 20 && "
    "ip netns exec tr1 ip link add link mvtr name mvtr.30 type vlan id 30 && "
    "ip netns exec tr1 ip link set mvtr.20 up && ip netns exec tr1 ip link set mvtr.30 up"
)
lease("tr1", "mvtr.20")
lease("tr1", "mvtr.30")

print("--- 2: native (untagged) network on the same trunk NIC, via a macvlan in a netns ---")
macvlan_ns("nat", "eth2", "mvnat", "52:54:00:17:00:02")
lease("nat", "mvnat")

print("--- 3: host VLAN device eth2.20, then a block stand-in: macvlan of eth2.20 in a netns ---")
n1.succeed("ip link add link eth2 name eth2.20 type vlan id 20 && ip link set eth2.20 up")
macvlan_ns("blk", "eth2.20", "mvblk", "52:54:00:17:00:03")
rc, first = lease("blk", "mvblk")
print(n1.execute("ip netns exec blk ping -c2 -W2 10.20.0.1")[1])
print("isolation: the block netns has no route to VLAN 30 or the management network")
print(n1.execute("ip netns exec blk ping -c1 -W2 10.30.0.1")[1])
print(n1.execute("ip netns exec blk ping -c1 -W2 192.168.1.1")[1])

print("--- 4: re-create the block netns with the same MAC; is the lease the same? ---")
n1.succeed("ip netns del blk")
macvlan_ns("blk", "eth2.20", "mvblk", "52:54:00:17:00:03")
rc, second = lease("blk", "mvblk")
print(f"same-address={first.split()[3:4] == second.split()[3:4]}")

print("--- 5: trunk child again, now that the host has eth2.20 (does the host take VID 20 first?) ---")
n1.succeed("ip netns exec tr1 ip -4 addr flush dev mvtr.20")
macvlan_ns("tr2", "eth2", "mvtr2", "52:54:00:17:00:04")
n1.succeed(
    "ip netns exec tr2 ip link add link mvtr2 name mvtr2.20 type vlan id 20 && "
    "ip netns exec tr2 ip link add link mvtr2 name mvtr2.30 type vlan id 30 && "
    "ip netns exec tr2 ip link set mvtr2.20 up && ip netns exec tr2 ip link set mvtr2.30 up"
)
lease("tr2", "mvtr2.20")
lease("tr2", "mvtr2.30")

print("--- dnsmasq log and tagged frames seen on n2 ---")
print(n2.succeed("journalctl -u dnsmasq --no-pager | grep -E 'DHCP(ACK|OFFER)' || true"))
print(n2.succeed("for v in 20 30; do echo vlan $v: $(grep -c \"vlan $v\" /tmp/tagged.txt); done"))
print(n1.succeed("journalctl -u dhcpcd --no-pager | grep -E 'eth2|mv' || true"))
