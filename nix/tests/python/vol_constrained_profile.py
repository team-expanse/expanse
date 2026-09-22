"""R9 investigation: a real CPU profile of expansed's idle overhead in the exact
E7/vol-constrained scenario (4 GB RAM, 2 cores, one data disk, a healthy replication-3
volume attached) — the measurement vol-constrained's `top -H` sample could not explain on
its own. Runs after cluster-common.py and vol_cluster.py.
"""

PROFILE_SECONDS = 25


def agent_pid(m):
    return m.succeed("systemctl show -p MainPID --value expansed.service").strip()


form_start = time.time()
form("test")
print(f"cluster formed in {time.time() - form_start:.1f}s on 4 GB RAM / 2 cores")

n1.succeed("expanse ctl volume create cvol --size 512Mi --replication 3")
res = None
with subtest("a replicated volume goes healthy on the constrained node"):
    wait_for(lambda: (volume_row(n1, "cvol") or {}).get("state") == "healthy", "cvol to be Healthy", 180)
    res = volume_row(n1, "cvol")["id"]
    wait_for(lambda: len(primaries(res)) == 1, "one primary of cvol")
    wait_for(lambda: fully_replicated(primaries(res)[0], res), "cvol's replicas to be UpToDate", 180)
    node = primaries(res)[0]
    print(f"cvol healthy, primary {node.name}")

with subtest("capture a real CPU profile of the idle primary via net/http/pprof"):
    pid = agent_pid(node)
    node.succeed(
        f"curl -sf 'http://127.0.0.1:6060/debug/pprof/profile?seconds={PROFILE_SECONDS}' "
        f"-o /tmp/cpu.prof",
        timeout=PROFILE_SECONDS + 30,
    )
    size = int(node.succeed("stat -c %s /tmp/cpu.prof").strip())
    assert size > 0, "empty pprof profile"
    print(f"captured {size} bytes over {PROFILE_SECONDS}s on {node.name} (pid {pid})")

    # CGO_ENABLED=0: `go tool pprof` isn't a prebuilt GOROOT binary here, so its first
    # invocation compiles itself, which otherwise pulls in runtime/cgo and needs a C
    # compiler this VM doesn't have (matches the repo-wide CGO_ENABLED=0 convention).
    top = node.succeed(f"CGO_ENABLED=0 go tool pprof -top -nodecount=30 {EXPANSE_BIN} /tmp/cpu.prof 2>&1")
    print(f"=== CPU profile top 30, {node.name}, {PROFILE_SECONDS}s idle window ===\n{top}")

    tree = node.succeed(f"CGO_ENABLED=0 go tool pprof -text -nodecount=30 -cum {EXPANSE_BIN} /tmp/cpu.prof 2>&1")
    print(f"=== CPU profile top 30 by cumulative time ===\n{tree}")

    goroutines = node.succeed("curl -sf 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1'")
    print(f"=== goroutine dump, {node.name} ===\n{goroutines}")

print("VOL-CONSTRAINED-PROFILE DONE")
