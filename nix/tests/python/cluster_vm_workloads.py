"""NODE_COUNT nodes, two VM workloads serving HTTP from replicated disks, observed from an outside watcher.

Measures cluster forming, placement, failover when a workload's node dies, the dead node's rejoin
and resync, and failover when the raft leader dies. Runs after cluster-common.py and
block-common.py; expects KERNEL, INITRD, CMDLINE, POLLER and NODE_COUNT spliced in.
"""
import functools

print = functools.partial(print, flush=True)  # stdout is a file; show timings as they happen

NODES = {f"n{i}": globals()[f"n{i}"] for i in range(1, NODE_COUNT + 1)}
QUORUM = (NODE_COUNT, NODE_COUNT // 2 + 1)

WORKLOADS = {"web1": "192.168.1.50", "web2": "192.168.1.51"}
TIMINGS = []


def note(what, seconds):
    TIMINGS.append((what, seconds))
    print(f"TIMING {what}: {seconds:.1f}s")


# --- cluster state ---

def quorum(m):
    found = re.search(r"quorum:\s+(\d+)/(\d+)", status(m))
    return (int(found.group(1)), int(found.group(2))) if found else (0, 0)


def leader(m):
    return leader_of(status(m))


def until(predicate, what, timeout):
    """Poll predicate every half second; return seconds taken."""
    t0 = time.time()
    while time.time() - t0 < timeout:
        if predicate():
            return time.time() - t0
        time.sleep(0.5)
    raise AssertionError(f"timed out after {timeout}s waiting for {what}")


def manifest(name, ip):
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
      size: 64Mi
      replication: 3
      mountPath: /mnt/{name}-disk
      filesystem: none
  placement:
    requiredCapabilities: [kvm]
  config:
    bootKernel: "{KERNEL}"
    bootInitrd: "{INITRD}"
    bootCmdline: "{CMDLINE} workload.name={name} workload.ip={ip}"
"""


def holder(name, via):
    b = get_json(via, name)
    nodes = placement_nodes(b) if b else set()
    return next(iter(nodes)) if len(nodes) == 1 else None


# --- the watcher's view ---

def poll_log():
    out = watcher.succeed("cat /tmp/poll.log")
    rows = []
    for line in out.splitlines():
        parts = line.split(" ", 3)
        if len(parts) >= 3:
            rows.append((int(parts[0]) / 1000, parts[1], parts[2] == "ok", parts[3] if len(parts) > 3 else ""))
    return rows


def field(body, key):
    found = re.search(rf"{key}=(\d+)", body)
    return int(found.group(1)) if found else None


def outage(name, since):
    """(seconds without a successful poll after `since`, last seq before, first seq after), or None."""
    rows = [r for r in poll_log() if r[1] == name]
    before = [r for r in rows if r[0] <= since and r[2]]
    after = [r for r in rows if r[0] > since]
    last_ok = before[-1] if before else None
    fail = next((r for r in after if not r[2]), None)
    if fail is None:
        return None
    back = next((r for r in after if r[2] and r[0] > fail[0]), None)
    if back is None or last_ok is None:
        return None
    return back[0] - last_ok[0], field(last_ok[3], "seq"), field(back[3], "seq"), field(back[3], "boot")


def serving(name):
    rows = [r for r in poll_log()[-8:] if r[1] == name]
    return bool(rows) and rows[-1][2]


def dump_path(name, via):
    """Print the watcher-to-guest network state, so a workload that never serves says why."""
    ip = WORKLOADS[name]
    print(watcher.execute(f"ip neigh show {ip}; curl -sv --max-time 2 http://{ip}/ 2>&1 | tail -5")[1])
    node = NODES.get(holder(name, via) or "")
    if node is None:
        print(f"DIAG {name}: no single holder")
        return
    print(node.execute(
        f"ip -d -s link show type macvtap; ip -s link show eth1; "
        f"journalctl -u expanse-block-root@default-{name}-0 -n 15 --no-pager")[1])


def until_serving(name, what, timeout, via):
    try:
        return until(lambda: serving(name), what, timeout)
    except AssertionError:
        dump_path(name, via)
        raise


def report_outage(name, since, label, via):
    until_serving(name, f"{name} serving before the {label} report", 600, via)
    time.sleep(3)
    o = outage(name, since)
    if o is None:
        print(f"OUTAGE {name} ({label}): none seen by the watcher")
        note(f"{label}: {name} outage", 0.0)
        return
    gap, seq_before, seq_after, boot = o
    lost = (seq_before or 0) - (seq_after or 0)  # the first page back may repeat the last one seen
    print(f"OUTAGE {name} ({label}): {gap:.1f}s; last seq before {seq_before}, first after {seq_after} "
          f"(boot {boot}); acknowledged writes lost: {max(lost, 0)}")
    note(f"{label}: {name} outage (watcher)", gap)


# --- scenario ---

t_boot = time.time()
for m in list(NODES.values()) + [watcher]:
    m.start()
for m in NODES.values():
    m.wait_for_unit("multi-user.target")
    m.succeed("systemctl stop expansed.service")
note(f"{NODE_COUNT} nodes booted", time.time() - t_boot)

with subtest(f"form a {NODE_COUNT}-node cluster"):
    t0 = time.time()
    n1.succeed("expanse cluster init --data-dir /persist/expanse --name workloads --node-id n1 "
               f"--advertise-addr {addr(n1)}:7444 --expect {NODE_COUNT}")
    token = ""
    for _ in range(30):
        _, out = n1.execute(f"expanse cluster token --data-dir /persist/expanse create --uses {NODE_COUNT - 1} 2>/dev/null || true")
        found = re.search(r"expanse-join-[A-Za-z0-9_-]+", out)
        if found:
            token = found.group(0)
            break
        time.sleep(1)
    assert token, "no join token minted"
    n1.succeed("systemctl start expansed.service")
    wait_agent_ready(n1)
    note("form: init + first node serving", time.time() - t0)
    for name in list(NODES)[1:]:
        tj = time.time()
        join_and_start(NODES[name], token, "voter")
        note(f"form: {name} joined", time.time() - tj)
    until(lambda: quorum(n1) == QUORUM, f"quorum {QUORUM}", 180)
    note(f"form: total, init to quorum {QUORUM[0]}/{QUORUM[1]}", time.time() - t0)
    print(status(n1))

with subtest("deploy two VM workloads and serve HTTP from their disks"):
    watcher.succeed(f"systemd-run --unit poller --setenv=PATH=/run/current-system/sw/bin sh -c 'sh {POLLER} "
                    + " ".join(f"{n}={ip}" for n, ip in WORKLOADS.items()) + " > /tmp/poll.log'")
    t0 = time.time()
    for name, ip in WORKLOADS.items():
        deploy(n1, name, manifest(name, ip))
    for name in WORKLOADS:
        wait_phase(n1, name, ["RUNNING"], 600)
        note(f"deploy: {name} RUNNING", time.time() - t0)
    for name in WORKLOADS:
        until_serving(name, f"{name} serving HTTP", 600, n1)
        note(f"deploy: {name} serving HTTP to the watcher", time.time() - t0)
    placement = {name: holder(name, n1) for name in WORKLOADS}
    print(f"PLACEMENT {placement}; leader {leader(n1)}")
    assert placement["web1"] != placement["web2"], f"both workloads placed on {placement['web1']}"
    print(n1.succeed("expanse ctl volume list 2>&1"))
    time.sleep(20)  # a steady baseline in the poll log

with subtest("hard-kill the node running web1"):
    victim = placement["web1"]
    was_leader = leader(n1) == victim
    via = next(m for n, m in NODES.items() if n != victim)
    t_kill = time.time()
    NODES[victim].crash()
    print(f"KILLED {victim} (holding web1{', and the raft leader' if was_leader else ''}) at {t_kill:.1f}")
    if was_leader:
        note("node loss: new leader elected", until(lambda: leader(via) not in ("", victim), "a new leader", 120))
    t = until(lambda: holder("web1", via) not in (None, victim), "web1 rescheduled", 600)
    note("node loss: web1 rescheduled", t)
    until_serving("web1", "web1 serving again", 600, via)
    note("node loss: web1 serving again, from the kill", time.time() - t_kill)
    time.sleep(10)
    report_outage("web1", t_kill, "node loss", via)
    report_outage("web2", t_kill, "node loss", via)
    placement = {name: holder(name, via) for name in WORKLOADS}
    print(f"PLACEMENT {placement}; leader {leader(via)}")

with subtest("bring the dead node back: rejoin and resync"):
    t0 = time.time()
    NODES[victim].start()
    NODES[victim].wait_for_unit("multi-user.target")
    wait_agent_ready(NODES[victim])
    note("rejoin: agent up after power-on", time.time() - t0)
    until(lambda: quorum(via) == QUORUM, "full quorum again", 300)
    note("rejoin: back in quorum", time.time() - t0)
    rc = until(lambda: NODES[victim].execute(
        "s=$(drbdadm status 2>&1); echo \"$s\" | grep -q disk: && ! echo \"$s\" | grep -Eq 'Inconsistent|Outdated|Sync|Connecting'")[0] == 0,
        "the node's DRBD replicas UpToDate", 600)
    note("rejoin: its DRBD replicas UpToDate", time.time() - t0)
    print(NODES[victim].succeed("drbdadm status 2>&1"))

with subtest("hard-kill the raft leader"):
    lead = leader(via)
    hosts = [w for w, n in placement.items() if n == lead]
    survivor = next(m for n, m in NODES.items() if n != lead)
    t_kill = time.time()
    NODES[lead].crash()
    print(f"KILLED leader {lead} (holding {hosts or 'no workload'}) at {t_kill:.1f}")
    note("leader loss: new leader elected", until(lambda: leader(survivor) not in ("", lead), "a new leader", 120))
    for w in hosts:
        until(lambda w=w: holder(w, survivor) not in (None, lead), f"{w} rescheduled", 600)
        until_serving(w, f"{w} serving again", 600, survivor)
        note(f"leader loss: {w} serving again, from the kill", time.time() - t_kill)
    time.sleep(20)  # long enough for a survivor to act on a failed desired-state read
    for w in WORKLOADS:
        report_outage(w, t_kill, "leader loss", survivor)
    print(f"leader now {leader(survivor)}")

print("SUMMARY")
for what, seconds in TIMINGS:
    print(f"SUMMARY {seconds:8.1f}s  {what}")
print(watcher.succeed("tail -4 /tmp/poll.log"))
