"""net-multi-nic (Phase 13 B1): with four NICs and DHCP on eno3 alone, everything lands on eno3.

eno1 has carrier but no DHCP server (dhcpcd keeps soliciting and assigns nothing); eno2 and eno4
have no carrier; bmc holds the link-local 169.254.0.2 of a Dell iDRAC's USB NIC. The advertise
guess must pass over bmc, and a cluster formed on its address (as releases before 1.16 could)
moves to eno3's. Asserts the default route, the raft advertise address, the web UI's VIP and a VM
block's macvtap all sit on eno3, and that the router reaches the VIP and the guest.

Runs after cluster-common.py, block-common.py, vol_cluster.py and single_node_common.py.
Expects KERNEL, INITRD and CMDLINE (net-multi-nic.nix) spliced in ahead of this file.
"""

HOST_IP = "192.168.3.10"
BMC_IP = "169.254.0.2"
VIP = "192.168.3.200"
GUEST_MAC = "52:54:00:aa:00:01"
GUEST_IP = "192.168.3.20"


def guest_manifest(name):
    return f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {name}
  namespace: default
spec:
  type: vm/instance
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 1000m
      memory: 2Gi
  storage:
    - name: disk
      size: 16Mi
      mountPath: /mnt/{name}-disk
      filesystem: none
  placement:
    requiredCapabilities: [kvm]
  config:
    mac: "{GUEST_MAC}"
    guestReady: none
    bootKernel: "{KERNEL}"
    bootInitrd: "{INITRD}"
    bootCmdline: "{CMDLINE}"
"""


def node_record():
    rc, out = n1.execute("expanse ctl kv --socket /run/expanse/agent.sock get /nodes/n1 2>/dev/null")
    found = re.search(r"\{.*\}", out, re.S)
    return json.loads(found.group(0)) if rc == 0 and found else {}


def addrs(dev):
    return n1.succeed(f"ip -4 -o addr show dev {dev}")


router.start()
router.wait_for_unit("dnsmasq.service")
n1.start()
# QEMU numbers netdevs by NIC order, not VLAN: bmc is vlan1, so eno2 and eno4 are vlan3 and vlan5.
for netdev in ("vlan3", "vlan5"):
    n1.send_monitor_command(f"set_link {netdev} off")
n1.wait_for_unit("multi-user.target")

with subtest("only eno3 gets an IPv4 address"):
    wait_for(lambda: HOST_IP in addrs("eno3"), "eno3's DHCP lease", timeout=120)
    print(n1.succeed("ip -4 -o addr; ip -4 route"))
    for dev in ("eno1", "eno2", "eno4"):
        assert "inet " not in addrs(dev), f"{dev} has no DHCP server but holds an address: {addrs(dev)}"

with subtest("the default route is on eno3 alone"):
    routes = n1.succeed("ip -4 route show default").strip().splitlines()
    assert routes and all("dev eno3" in r for r in routes), f"default routes: {routes}"

with subtest("the advertise guess passes over the BMC's link-local address"):
    assert f"inet {BMC_IP}/" in addrs("bmc"), addrs("bmc")
    usage = n1.succeed("expanse cluster init --help")
    assert f'"{HOST_IP}:7444"' in usage, f"init would advertise another address:\n{usage}"

with subtest("a cluster formed on the BMC's address moves to eno3's"):
    n1.succeed("systemctl stop expansed.service")
    n1.succeed("expanse cluster init --data-dir /persist/expanse --name multinic --node-id n1 --expect 1 "
               f"--advertise-addr {BMC_IP}:7444")
    n1.succeed("systemctl start expansed.service")
    wait_agent_ready(n1)
    wait_one_node_quorum()
    raft_addr = n1.succeed("cat /persist/expanse/raft-addr")
    assert f"{HOST_IP}:7444" in raft_addr, f"raft advertises {raft_addr!r}, want {HOST_IP}:7444"
    wait_for(lambda: node_record().get("raft_addr") == f"{HOST_IP}:7444", "the node record's repair", 60)
    n1.succeed(f"journalctl -u expansed | grep -F 'advertise address repaired' | grep -F {HOST_IP}")
    n1.succeed("systemctl restart expansed.service")
    wait_agent_ready(n1)
    wait_one_node_quorum()
    assert BMC_IP not in n1.succeed("expanse cluster status"), "status still shows the BMC's address"

with subtest("the web UI's VIP is on eno3 and the router reaches it"):
    wait_for(lambda: VIP in addrs("eno3"), "the UI VIP on eno3", timeout=60)
    for dev in ("eno1", "eno2", "eno4"):
        assert VIP not in addrs(dev), f"VIP also on {dev}"
    router.wait_until_succeeds(f"curl -sk -o /dev/null https://{VIP}:8443/login", timeout=60)

with subtest("a VM block's macvtap sits on eno3 and its guest gets the reserved lease"):
    deploy(n1, "guest", guest_manifest("guest"))
    wait_phase(n1, "guest", ["RUNNING"], 600)
    links = n1.succeed("ip -o link show type macvtap")
    print(links)
    assert links.strip() and all("@eno3" in ln for ln in links.strip().splitlines()), links
    router.wait_until_succeeds(f"grep -i {GUEST_MAC} /var/lib/dnsmasq/dnsmasq.leases", timeout=300)
    router.wait_until_succeeds(f"ping -c1 -W2 {GUEST_IP}", timeout=120)

with subtest("the node's DHCP client leaves the guest's macvtap alone"):
    # Each check spans dhcpcd's solicit window: on 1.10.0 it leased the guest's reservation to the node.
    for _ in range(10):
        # `ip addr show type macvtap` ignores the type filter, so list the links first.
        mv = n1.succeed("for l in $(ip -o link show type macvtap | awk -F'[ :@]+' '{print $2}'); do ip -4 -o addr show dev $l; done")
        assert "inet " not in mv, f"the node holds an address on the guest's macvtap:\n{mv}"
        time.sleep(3)
    router.succeed(f"ip neigh flush to {GUEST_IP}; ping -c1 -W2 {GUEST_IP}")
    neigh = router.succeed(f"ip neigh show to {GUEST_IP}")
    assert GUEST_MAC in neigh.lower(), f"{GUEST_IP} answered by another MAC: {neigh}"

print("NET-MULTI-NIC DONE")
