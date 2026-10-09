"""vol-drbd-config testScript body (Phase 1 B2).

Feeds every golden .res file rendered by internal/storage/drbd to the real
`drbdadm dump`, and checks DRBD parsed the D7 policy rather than ignoring it.
"""

import re

start_all()
n1.wait_for_unit("multi-user.target")

REPLICAS = [1, 2, 3, 5]
# The after-sb policies by name; a bare "discard-" also matches rs-discard-granularity.
NEVER = [
    "discard-younger-primary", "discard-older-primary", "discard-zero-changes",
    "discard-least-changes", "discard-local", "discard-remote", "discard-secondary",
    "discard-node-", "consensus", "violently", "call-pri-lost", "auto-discard",
]

n1.succeed("mkdir -p /tmp/cfg")
for count in REPLICAS:
    n1.copy_from_host(f"@golden@/{count}-replicas.res", f"/tmp/cfg/{count}-replicas.res")

for count in REPLICAS:
    with subtest(f"{count} replicas parse"):
        dumped = n1.succeed(f"drbdadm -c /tmp/cfg/{count}-replicas.res dump vol-a1")
        print(dumped)
        assert "protocol" in dumped and "C;" in dumped, "protocol C missing"
        assert "minor 3" in dumped, "minor lost"
        assert "meta-disk" in dumped and "internal" in dumped, "internal metadata lost"
        for policy in ("after-sb-0pri", "after-sb-1pri", "after-sb-2pri", "rr-conflict"):
            assert re.search(rf"{policy}\s+disconnect;", dumped), f"{policy} disconnect not parsed"
        assert "split-brain" in dumped, "split-brain handler not parsed"
        assert dumped.count("node-id") == count, f"expected {count} node-ids"
        for banned in NEVER:
            assert banned not in dumped, f"destructive policy {banned} in dump"
        quorum = "majority" if count >= 3 else "off"
        assert re.search(rf"quorum\s+{quorum};", dumped), f"quorum {quorum} not parsed"
        assert re.search(r"c-min-rate\s+4M;", dumped), "the resync floor (c-min-rate 4M) not parsed"

print("VOL-DRBD-CONFIG COMPLETED")
