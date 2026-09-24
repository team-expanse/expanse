# Probe body for vm-macvtap-probe.nix (Phase 6 D2). Split into its own
# file per CLAUDE.md so it can be linted before a derivation build.
n1.start()
n2.start()
n1.wait_for_unit("multi-user.target")
n2.wait_for_unit("multi-user.target")
n1.wait_until_succeeds("ping -c1 -W2 192.168.1.2")

print("--- baseline: n1's real (only) NIC reachable from n2 before any macvtap change ---")
print(n2.succeed("ping -c2 -W2 192.168.1.1"))

print("--- create macvtap0 (mode bridge) on n1's eth1, move to netns 'guestns' as a stand-in guest identity ---")
n1.succeed(
    "ip netns add guestns && "
    "ip link add link eth1 name macvtap0 type macvtap mode bridge && "
    "ip link set macvtap0 netns guestns && "
    "ip netns exec guestns ip link set macvtap0 address 52:54:00:aa:bb:01 up && "
    "ip netns exec guestns ip addr add 192.168.1.50/24 dev macvtap0"
)

print("--- R2: is n1's own existing traffic on eth1 disrupted by the macvtap child? ---")
print(n2.succeed("ping -c2 -W2 192.168.1.1"))

print("--- X3: is the macvtap child (stand-in guest) reachable from another node over the real LAN segment? ---")
print(n2.succeed("ping -c2 -W2 192.168.1.50"))

print("--- known limitation: can n1's own root netns reach its macvtap child directly over eth1? ---")
rc, out = n1.execute("ping -c2 -W2 192.168.1.50")
print(f"exit={rc}\n{out}")

print("--- workaround: a second bridge-mode macvtap sibling, used by the host instead of eth1 directly ---")
n1.succeed(
    "ip link add link eth1 name macvtap-host type macvtap mode bridge && "
    "ip link set macvtap-host address 52:54:00:aa:bb:02 up && "
    "ip addr add 192.168.1.51/24 dev macvtap-host"
)
rc, out = n1.execute("ping -c2 -W2 -I macvtap-host 192.168.1.50")
print(f"host-via-sibling-macvtap exit={rc}\n{out}")

print("--- after both macvtap children exist, is n1's original eth1 address still reachable from n2? ---")
print(n2.succeed("ping -c2 -W2 192.168.1.1"))
