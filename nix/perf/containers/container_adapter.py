"""Adapter giving cluster-common.py's / idle_main.py's
nixosTest-driver-shaped calls (m.succeed/execute/wait_for_unit, start_all(), subtest())
a real backend on THIS host: systemd-nspawn containers managed via `nsenter`/`machinectl`,
instead of a nixosTest driver's QEMU machines -- see run.py.
"""
import contextlib
import subprocess
import time


class Container:
    def __init__(self, name):
        self.name = name

    def execute(self, cmd):
        """Matches nixosTest's Machine.execute(): returns (exit_status, combined output).
        nsenter's directly into the leader's namespaces rather than going through
        `nixos-container run` (which shells commands through `su root -l -c ...`) --
        `su`'s PAM account-phase check fails here ("helper binary execve failed": the
        unix_chkpwd setuid wrapper isn't available inside this unprivileged container),
        and even though su still succeeds functionally, the warning it prints pollutes
        stdout/stderr enough to break exact-match assertions like wait_for_unit's, found
        live. nsenter needs no PAM/su at all -- the same mechanism setup.sh's own
        readiness check already uses cleanly."""
        pid = self._leader_pid()
        if pid is None:
            return 1, f"{self.name}: no leader PID (container not running?)"
        p = subprocess.run(
            ["nsenter", "--target", str(pid), "--all", "--", "sh", "-c", cmd],
            capture_output=True, text=True,
        )
        return p.returncode, p.stdout + p.stderr

    def succeed(self, cmd):
        rc, out = self.execute(cmd)
        if rc != 0:
            raise AssertionError(f"{self.name}: {cmd!r} failed (rc={rc}): {out}")
        return out

    def wait_for_unit(self, unit, timeout=120):
        deadline = time.time() + timeout
        last = ""
        while time.time() < deadline:
            rc, out = self.execute(f"systemctl is-active {unit}")
            last = out.strip()
            if last == "active":
                return
            time.sleep(1)
        raise AssertionError(f"{self.name}: {unit} never became active (last: {last!r})")

    def _leader_pid(self):
        out = subprocess.run(
            ["machinectl", "show", self.name, "-p", "Leader", "--value"],
            capture_output=True, text=True,
        ).stdout.strip()
        return int(out) if out.isdigit() else None

    def start(self):
        """`systemctl start container@<name>.service`, not `machinectl start <name>`:
        the container is declared via NixOS's own containers.<name> module (autoStart =
        false), whose container@.service unit is what actually populates and registers
        the machine on first start. `machinectl start` only works on an already-
        registered machine image and fails "Machine image '<name>' does not exist" the
        first time -- found live on the real host, not assumed."""
        subprocess.run(["systemctl", "start", f"container@{self.name}.service"], check=True)


@contextlib.contextmanager
def subtest(name):
    print(f"--- {name} ---", flush=True)
    yield


def start_all():
    for m in (n1, n2, n3):
        active = subprocess.run(
            ["systemctl", "is-active", f"container@{m.name}.service"],
            capture_output=True, text=True,
        ).stdout.strip()
        if active != "active":
            m.start()
    for m in (n1, n2, n3):
        m.wait_for_unit("multi-user.target", timeout=180)


n1 = Container("n1")
n2 = Container("n2")
n3 = Container("n3")
NODES = [n1, n2, n3]
