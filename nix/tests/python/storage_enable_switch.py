"""storage-enable-switch (1.10.1): turn storage on with a switch, as docs/STORAGE.md §2 says, no reboot.

On 1.10.0 the documented lvcreate failed (dm_thin_pool only in the next boot's initrd), and volume
create wrote DRBD 9 resources that the in-tree DRBD 8.4 loaded by the switch could not parse.

Runs after cluster-common.py, block-common.py, vol_cluster.py and single_node_common.py.
"""

form_single("storage-switch")
n1.wait_for_unit("data-vg.service")

with subtest("a switch that enables storage loads dm_thin_pool, so the thin pool can be made at once"):
    n1.succeed("/run/current-system/specialisation/storage/bin/switch-to-configuration test")
    n1.succeed("lvcreate --type thin-pool -l 90%FREE -n pool expanse")

with subtest("with DRBD 8.4 loaded, volume create says to reboot and writes no resource"):
    print(n1.succeed("head -1 /proc/drbd || true"))
    n1.wait_until_succeeds("systemctl is-active expansed.service", timeout=60)
    wait_agent_ready(n1)
    n1.succeed("expanse ctl volume create v1 --size 64Mi --replication 1")
    n1.wait_until_succeeds(
        "journalctl -u expansed.service --no-pager | grep -q 'volumes need DRBD 9; reboot the node'", timeout=120)
    n1.fail("ls /etc/drbd.d/*.res")

print("STORAGE-ENABLE-SWITCH DONE")
