# Shared Python helpers for the Phase 04 block VM tests (§8).
# Spliced into each test's testScript via readFile; assumes the Phase 03
# cluster-common.py helpers were spliced first (form/join_and_start/
# wait_quorum/IP). The blocks flake and catalog are shipped to every
# node by the test's NixOS config (environment.etc).
import json

SOCK = "--socket /run/expanse/agent.sock"

# Valid util/echo manifest with anti-affinity (each replica on its own
# node; the canonical §8 test block). `body` distinguishes generations
# in rolling tests; `port` must be free on every node.
def echo_yaml(name="web", replicas=3, port=18080, body="hi\n", antiaffinity=True,
              strategy=None):
    aa = "  placement:\n    antiAffinity: ANTI_AFFINITY_NODE\n" if antiaffinity else ""
    # strategy: None (default active-active), "SINGLETON" (V5: replicas
    # must be 1) or "DAEMONSET" (V6: replicas key must be absent).
    st = ""
    if strategy is not None:
        st = f"  strategy:\n    kind: {strategy}\n"
    reps = f"  replicas: {replicas}\n" if replicas is not None else ""
    return (
        "apiVersion: expanse.io/v1\nkind: Block\n"
        f"metadata:\n  name: {name}\n  namespace: default\n"
        "spec:\n  type: util/echo\n"
        f"{reps}{st}{aa}"
        "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
        "  config:\n"
        f"    port: {port}\n"
        f"    body: {json.dumps(body)}\n"
    )


import base64


def deploy(m, name, yaml_text, port=18080):
    """Apply a manifest on the leader node; idempotent retries. The
    manifest travels base64-encoded — no heredoc/pty mangling."""
    path = f"/tmp/{name}.yaml"
    enc = base64.b64encode(yaml_text.encode()).decode()
    m.succeed(f"echo {enc} | base64 -d > {path}")
    m.succeed(f"expanse ctl block apply {SOCK} -f {path}")


def get_json(m, name, ns="default"):
    rc, out = m.execute(f"expanse ctl block get {SOCK} -n {ns} -o json {name} || true")
    # The driver may merge stderr into the output; take the JSON document.
    start = out.find("{")
    if rc != 0 or start < 0:
        print(f"DBG {name}: rc={rc} out={out!r}")
        return None
    try:
        return json.loads(out[start:out.rfind("}") + 1])
    except json.JSONDecodeError:
        return None


def wait_phase(m, name, want_phases, timeout, ns="default"):
    """Poll until the block phase is in want_phases; return the JSON.
    want_phases: list of phase names, e.g. ["RUNNING"]."""
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        b = get_json(m, name, ns)
        if b is not None:
            last = b
            phase = (b.get("status") or {}).get("phase", "")
            if phase in want_phases:
                return b
        time.sleep(2)
    phase = ((last or {}).get("status") or {}).get("phase", "?")
    raise Exception(f"block {name} never reached {want_phases}; last phase={phase}; status={last}")


def placement_nodes(b):
    """Distinct node IDs holding live (index >= 0, not LOST) placements.
    NOTE: protojson omits zero-valued fields — a missing replicaIndex is 0."""
    nodes = set()
    for p in (b.get("status") or {}).get("placements", []):
        if p.get("replicaIndex", 0) < 0 or p.get("phase") == "LOST":
            continue
        nodes.add(p.get("nodeId"))
    return nodes


def wait_placement_nodes(m, name, count, timeout, ns="default"):
    """Poll until the block has live placements on count nodes; return the JSON."""
    deadline = time.time() + timeout
    b = None
    while time.time() < deadline:
        b = get_json(m, name, ns)
        if b is not None and len(placement_nodes(b)) == count:
            return b
        time.sleep(2)
    raise Exception(f"block {name} never on {count} nodes: {(b or {}).get('status')}")


def replica_phases(b):
    return {p.get("replicaIndex", 0): p.get("phase")
            for p in (b.get("status") or {}).get("placements", [])}


def echo_responds(m, port, body=None, node=None):
    """Assert the echo server answers on :port; optionally check body."""
    target = f"http://{node}:{port}/" if node else f"http://localhost:{port}/"
    rc, out = m.execute(f"curl -sS --max-time 3 {target}")
    if rc != 0 or not out:
        raise Exception(f"echo on {target} not responding (rc={rc}, out={out!r})")
    if body is not None and body not in out:
        raise Exception(f"echo body mismatch on {target}: want {body!r}, got {out!r}")


def unit_running(m, ns, name, idx):
    rc, out = m.execute(
        f"systemctl is-active expanse-block@{ns}-{name}-{idx}.service 2>/dev/null || true")
    return out.strip() == "active"


def replica_node(b, idx):
    """Node holding live placement idx, or None."""
    for p in (b.get("status") or {}).get("placements", []):
        if p.get("replicaIndex", 0) == idx and p.get("phase") != "LOST" and idx >= 0:
            return p.get("nodeId")
    return None


def wait_block_vip(m, name, timeout=60, ns="default"):
    """The external VIP allocated to a block; the pool's other address is the web UI's."""
    deadline = time.time() + timeout
    out = ""
    while time.time() < deadline:
        rc, out = kv(m, f"get /network/vipPool/external/{ns}/{name}")
        try:
            if rc == 0:
                return json.loads(out)["addr"].split("/")[0]
        except (ValueError, KeyError):
            pass
        time.sleep(2)
    raise AssertionError(f"{ns}/{name} was never given a VIP: {out!r}")
