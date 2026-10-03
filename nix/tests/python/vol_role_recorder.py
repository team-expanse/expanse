"""Records each node's DRBD role changes with `drbdsetup events2` and maps them onto the driver's clock.

Runs after cluster-common.py and vol_cluster.py; the wrapper imports vol_roles as `roles`.
"""

LOG = "/var/tmp/roles.log"
TOOLS = "/run/current-system/sw/bin"


def start_recorder(m):
    m.succeed(f"rm -f {LOG}")  # a transient unit appends to an old log instead of replacing it
    m.succeed(
        f"systemd-run --unit=roles --collect -p StandardOutput=file:{LOG} "
        f"{TOOLS}/stdbuf -oL {TOOLS}/drbdsetup events2 --timestamps all"
    )


def stop_recorder(m):
    m.succeed("systemctl stop roles.service")
    return m.succeed(f"cat {LOG}")


def offset_of(m):
    """m's clock minus the driver's, from the probe with the shortest round trip."""
    probes = []
    for _ in range(8):
        before = time.time()
        reading = float(m.succeed("date +%s.%N"))
        probes.append((before, reading, time.time()))
    return roles.clock_offset(probes)


def role_history(res, offsets, end):
    """Each node's Primary stretches in driver time, ending the recorders."""
    held = {}
    for m in NODES:
        history = roles.parse(stop_recorder(m), res)
        assert history, f"{m.name} recorded no role for {res}"
        held[m.name] = roles.primary_intervals([(when - offsets[m.name], role) for when, role in history], end=end)
    return held


def show(held, since):
    for name, stretches in held.items():
        print(f"{name}: Primary for {[(round(a - since, 1), round(b - since, 1)) for a, b in stretches]}")
