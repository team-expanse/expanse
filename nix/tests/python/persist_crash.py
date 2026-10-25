"""persist-crash: crash the node just after activation rewrote /var/lib/nixos; the next boot needs the maps.

A stage-1 probe logs whether the previous boot's uid-map was on disk before activation rewrote it.
"""

MAPS = ["uid-map", "gid-map", "auto-subuid-map"]


def check_maps():
    for name in MAPS:
        machine.succeed(f"python3 -c 'import json,sys; json.load(open(sys.argv[1]))' /var/lib/nixos/{name}")


machine.start()
machine.wait_for_unit("multi-user.target")
check_maps()

for boot in range(3):
    with subtest(f"crash {boot + 1}: the user maps survive and every user exists"):
        machine.crash()
        machine.start()
        machine.wait_for_unit("multi-user.target")
        machine.succeed("journalctl -b -o cat | grep -F 'persist-crash: uid-map kept'")
        machine.fail("journalctl -b -o cat | grep -F 'malformed JSON'")
        check_maps()
        machine.succeed("getent passwd sshd", "systemctl is-active sshd")

print("PERSIST-CRASH PASSED: user maps survived three crashes right after boot")
