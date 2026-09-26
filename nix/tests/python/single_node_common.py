"""Forming a one-node cluster on n1 (Phase 12). Runs after cluster-common.py and vol_cluster.py."""


def form_single(name="single", uses=0):
    """Init a cluster on n1 alone; with uses > 0, also mint a join token for later joiners
    (at /root/join-token), while the CLI still holds the store."""
    n1.start()
    n1.wait_for_unit("multi-user.target")
    n1.succeed("systemctl stop expansed.service")
    n1.succeed(
        "expanse cluster init --data-dir /persist/expanse "
        f"--name {name} --node-id n1 --advertise-addr 192.168.1.1:7444 --expect 1"
    )
    if uses:
        out = n1.succeed(f"expanse cluster token --data-dir /persist/expanse create --uses {uses}")
        found = re.search(r"expanse-join-[A-Za-z0-9_-]+", out)
        assert found, f"no join token minted: {out}"
        n1.succeed(f"echo -n '{found.group(0)}' > /root/join-token")
    n1.succeed("systemctl start expansed.service")
    wait_agent_ready(n1)
    wait_one_node_quorum()


def wait_one_node_quorum(timeout=90):
    wait_for(lambda: re.search(r"quorum:\s+1/1", status(n1)) and leader_of(status(n1)) == "n1",
             "n1 to lead a one-node quorum", timeout)
