#!/usr/bin/env python3
"""Host-side runner for the X1 real-hardware(-adjacent) idle CPU/RSS measurement,
against 3 systemd-nspawn containers on this bare-metal host instead of the nixosTest VM
harness's QEMU nodes -- see ./README.md for why, and ../../.plan/ARCHITECTURE.md §8's
node_control_plane_cpu_percent known_gap for the question this is actually answering.

Splices the same cluster-forming and measurement files nix/tests/vol-constrained.nix
splices via readFile, unmodified: only container_adapter.py (a container backend for
n1/n2/n3/subtest()/start_all()) and idle_main.py (the idle-only scenario) are new. This must
run as root (nsenter --all into a container's namespaces needs CAP_SYS_ADMIN); setup.sh
invokes it that way.
"""
import json
import os
import pathlib
import subprocess
import sys
import time

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parents[2]  # repo root
TESTS = ROOT / "nix" / "tests"

# root (this runs under sudo) doesn't inherit the invoking user's own
# ~/.config/nix/nix.conf, and /etc/nix/nix.conf has experimental-features empty --
# same fix as setup.sh's NIX_CONFIG, scoped to this process's own subprocess calls
# rather than written to any file.
os.environ["NIX_CONFIG"] = "experimental-features = nix-command flakes"


def start_logging():
    """Tee our fds 1 and 2 into a logfile, same as setup.sh/teardown.sh, so a long or
    failed run doesn't have to be pasted from terminal scrollback."""
    log_dir = pathlib.Path("/var/log/expanse-perf")
    log_dir.mkdir(parents=True, exist_ok=True)
    log_path = log_dir / f"run-{time.strftime('%Y%m%dT%H%M%S')}.log"
    tee = subprocess.Popen(["tee", "-a", str(log_path)], stdin=subprocess.PIPE)
    os.dup2(tee.stdin.fileno(), sys.stdout.fileno())
    os.dup2(tee.stdin.fileno(), sys.stderr.fileno())
    print(f"logging full output to {log_path}")


def budgets_json():
    out = subprocess.run(
        ["nix", "run", "nixpkgs#yq-go", "--", "-o=json", ".budgets", str(ROOT / "test/perf/budgets.yaml")],
        capture_output=True, text=True,
    )
    if out.returncode != 0:
        sys.exit(f"could not read test/perf/budgets.yaml: {out.stderr}")
    return out.stdout


def main():
    start_logging()
    src = "\n".join([
        (HERE / "container_adapter.py").read_text(),
        (TESTS / "cluster-common.py").read_text(),
        (TESTS / "python" / "vol_perf_lib.py").read_text(),
        (TESTS / "python" / "node_overhead.py").read_text(),
        f"BUDGETS = {budgets_json()}\n",
        (HERE / "idle_main.py").read_text(),
    ])
    exec(compile(src, "container-constrained", "exec"), {"__name__": "__main__"})


if __name__ == "__main__":
    main()
