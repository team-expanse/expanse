"""storage-enable-switch: turn storage on through the Nix override with a switch, no reboot.

Every node loads DRBD 9 at boot since 1.11, so a switch that sets storageVG gives working volumes at
once; 1.10 needed a reboot because the switch could only load the in-tree DRBD 8.4.
Runs after cluster-common.py, block-common.py, vol_cluster.py and single_node_common.py.
"""

form_single("storage-switch")
n1.wait_for_unit("data-vg.service")

with subtest("DRBD 9 is loaded before storage is configured"):
    n1.succeed("head -1 /proc/drbd | grep -q 'version: 9\\.'")

with subtest("a switch that enables storage lets the thin pool be made at once"):
    n1.succeed("/run/current-system/specialisation/storage/bin/switch-to-configuration test")
    n1.succeed("lvcreate --type thin-pool -l 90%FREE -n pool expanse")

with subtest("volumes work without a reboot, and the node reports its config"):
    n1.wait_until_succeeds("systemctl is-active expansed.service", timeout=60)
    wait_agent_ready(n1)
    n1.succeed("expanse ctl volume create v1 --size 64Mi --replication 1")
    wait_for(lambda: (volume_row(n1, "v1") or {}).get("state") == "healthy", "v1 healthy", 120)
    wait_for(lambda: '"source":"config"' in kv(n1, "get /storage-nodes/n1")[1], "a config storage report", 60)

print("STORAGE-ENABLE-SWITCH DONE")
