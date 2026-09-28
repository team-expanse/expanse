"""node-console: tty1 of an installed node shows the read-only host console.

The screen is read through /dev/vcs1 (wait_until_tty_matches), which is more reliable than
OCR. The node starts unclustered, then a one-node cluster is formed on it; a degraded md
mirror is built from the spare disks to check the mirror warning. tty2 and the serial
console must still offer a login.
"""

import re


def screen(pattern, timeout=60):
    machine.wait_until_tty_matches("1", pattern, timeout=timeout)


def tty1():
    return machine.get_tty_text("1")


def dump_on_failure():
    print("--- tty1 ---")
    print(tty1())
    print(machine.execute("journalctl -u expanse-console -u expansed -u expanse-identity --no-pager | tail -n 40")[1])
    print(machine.execute("findmnt /persist; ls -la /persist/expanse")[1])


machine.start()
machine.wait_for_unit("multi-user.target")
machine.wait_for_unit("expansed.service")
machine.wait_for_unit("expanse-console.service")

try:
    with subtest("tty1 shows the host console, not a login prompt"):
        screen("EXPANSE")
        screen("console-node")
        screen(r"Alt\+F2 for a login shell")
        screen(r"Node ID +[0-9a-f]{8}-")
        screen(r"https://[0-9.]+:8443")
        screen("not in a cluster yet")
        screen(r"expanse cluster init --expect 1")
        screen("HEALTHY|DEGRADED|UNHEALTHY")
        screen(r"CPU +.*threads")
        screen(r"Memory +[0-9.]+ [KMG]iB free of")
        assert "login:" not in tty1(), tty1()
        machine.screenshot("console-fresh")

    with subtest("a degraded md mirror is called out, under its /dev/md name"):
        machine.succeed("mdadm --create /dev/md/probe --run --metadata=1.2 --level=1 --raid-devices=2 /dev/vdb missing")
        screen(r"probe +md[0-9]+ raid1 \[U_\] +DEGRADED", timeout=30)
        machine.screenshot("console-degraded")
        machine.succeed("mdadm --stop /dev/md/probe")
        machine.succeed("mdadm --zero-superblock /dev/vdb")

    with subtest("with the agent stopped the console stays up and says so"):
        machine.succeed("systemctl stop expansed.service")
        screen("agent not running", timeout=30)
        machine.succeed("systemctl is-active expanse-console.service")

    with subtest("a one-node cluster shows its name, role and quorum"):
        machine.succeed("expanse cluster init --data-dir /persist/expanse --name lab --expect 1")
        machine.succeed("systemctl start expansed.service")
        machine.wait_for_unit("expansed.service")
        screen(r"Cluster +lab", timeout=120)
        screen(r"Role +leader")
        screen(r"Quorum +1/1")
        # Health comes from the heartbeat key, so it shows while the agent is up, cluster or not.
        screen(r"Health +(HEALTHY|DEGRADED|UNHEALTHY)", timeout=60)
        assert "agent not running" not in tty1(), tty1()
        machine.screenshot("console-clustered")

    with subtest("the console shows no secrets and offers no actions"):
        text = tty1().lower()
        for secret in ["password", "expanse-join-", "private", "token"]:
            assert secret not in text, f"{secret!r} on the console:\n{text}"
        machine.send_chars("ls\n")  # keys are read and ignored
        machine.send_key("ctrl-c")
        screen("EXPANSE")
        machine.succeed("systemctl is-active expanse-console.service")

    with subtest("tty2 and the serial console still offer a login"):
        machine.fail("systemctl is-active getty@tty1.service")
        machine.succeed("systemctl is-active getty.target")
        # The test framework masks serial-getty@ttyS0 itself (it owns the serial port), so the
        # serial login is checked by its unit files here and by node-config-eval on the real config.
        machine.succeed("systemctl list-unit-files serial-getty@.service | grep -q serial-getty@.service")
        machine.succeed("test -e /etc/systemd/system/getty@tty1.service")  # the tty1 getty is masked
        machine.send_key("alt-f2")
        machine.wait_until_tty_matches("2", "login:", timeout=60)
        machine.send_key("alt-f1")
        screen("EXPANSE")

    with subtest("the console survives a restart of itself and comes back on tty1"):
        machine.succeed("systemctl restart expanse-console.service")
        machine.wait_for_unit("expanse-console.service")
        screen("console-node")
        assert re.search(r"Alt\+F2", tty1()), tty1()
except Exception:
    dump_on_failure()
    raise

print("NODE-CONSOLE PASSED: tty1 host console, degraded mirror, agent down, cluster, tty2 + serial logins")
