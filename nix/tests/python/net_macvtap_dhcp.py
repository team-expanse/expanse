"""net-macvtap-dhcp (1.10.1): the node's DHCP client must leave VM macvtap children (mv*) alone.

On 1.10.0 dhcpcd leased the guest's reservation to the node over the child, and the node then answered
ARP and SSH for the guest's address.
"""
import time

GUEST_MAC = "02:00:00:00:de:01"
GUEST_IP = "192.168.1.60"
CHILD = "mv0e99594094fc"  # runVM's naming: "mv" + 12 hex

router.start()
router.wait_for_unit("dnsmasq.service")
n1.start()
n1.wait_for_unit("multi-user.target")

with subtest("a macvtap child made as runVM makes it"):
    n1.succeed(f"ip link add link eth1 name {CHILD} type macvtap mode bridge")
    n1.succeed(f"ip link set {CHILD} address {GUEST_MAC} up")

with subtest("dhcpcd takes no address on it, and the router leases nothing to the guest's MAC"):
    # Longer than dhcpcd's solicit-to-bound time on this segment (a few seconds on 1.10.0).
    for _ in range(10):
        addrs = n1.succeed(f"ip -4 -o addr show dev {CHILD}")
        assert "inet " not in addrs, f"the node took an address on {CHILD}: {addrs}"
        time.sleep(3)
    leases = router.succeed("cat /var/lib/dnsmasq/dnsmasq.leases || true")
    assert GUEST_MAC not in leases.lower() and GUEST_IP not in leases, f"leased to the node: {leases}"

print("NET-MACVTAP-DHCP DONE")
