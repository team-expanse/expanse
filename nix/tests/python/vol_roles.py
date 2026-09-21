"""Reads DRBD role history and checks that no two nodes were ever Primary at once.

Pure functions over `drbdsetup events2 --timestamps` output, so the VM scenario stays thin.
Times are seconds since the epoch; a node's clock is mapped onto the driver's with clock_offset.
"""

import re
from collections import namedtuple
from datetime import datetime
from itertools import combinations

Overlap = namedtuple("Overlap", "first second seconds")

_ROLE_LINE = re.compile(r"^(\S+) (?:exists|change) resource name:(\S+) role:(\w+)")


def parse(text, res):
    """(time, role) for each role report of res, in the order printed."""
    found = []
    for raw in text.splitlines():
        match = _ROLE_LINE.match(raw)
        if match and match.group(2) == res:
            found.append((datetime.fromisoformat(match.group(1)).timestamp(), match.group(3)))
    return found


def primary_intervals(events, end):
    """(start, stop) stretches in which the role was Primary; one still open is closed at end."""
    stretches = []
    start = None
    for when, role in events:
        if role == "Primary" and start is None:
            start = when
        elif role != "Primary" and start is not None:
            stretches.append((start, when))
            start = None
    if start is not None:
        stretches.append((start, end))
    return stretches


def _separation(a, b):
    """Seconds between two stretches; negative by the length of their overlap."""
    return max(b[0] - a[1], a[0] - b[1])


def _cross_node_pairs(held):
    for (x, xs), (y, ys) in combinations(held.items(), 2):
        for a in xs:
            for b in ys:
                yield x, a, y, b


def overlaps(held, tolerance):
    """Pairs of nodes whose Primary stretches overlap by more than tolerance seconds."""
    found = []
    for x, a, y, b in _cross_node_pairs(held):
        seconds = -_separation(a, b)
        if seconds > tolerance:
            found.append(Overlap(x, y, seconds))
    return found


def smallest_gap(held):
    """The closest approach between Primary stretches on different nodes, or None."""
    gaps = [_separation(a, b) for _, a, _, b in _cross_node_pairs(held)]
    return min(gaps) if gaps else None


def handoffs(held):
    """How many times the Primary moved from one node to another."""
    starts = sorted((start, node) for node, stretches in held.items() for start, _ in stretches)
    return sum(1 for (_, before), (_, after) in zip(starts, starts[1:]) if before != after)


def clock_offset(probes):
    """A node's clock minus the driver's, from (driver before, node time, driver after) probes.

    The probe with the shortest round trip pins the node's reading closest to the driver's midpoint.
    """
    before, node, after = min(probes, key=lambda p: p[2] - p[0])
    return node - (before + after) / 2
