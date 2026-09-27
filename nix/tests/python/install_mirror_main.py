"""install-mirror: the mirror layout puts the ESP and the btrfs system on md RAID1 across both disks.

The VM's own root is /dev/vda; the targets are /dev/vdb and /dev/vdc (install-mirror.nix).
"""

INSTALL = (
    "expanse install --config /etc/expanse-install.yaml --force --skip-system-install "
    "--target-flake /etc/expanse/flake >&2"
)
ESP_TYPE = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"  # firmware looks for this GPT type on each disk


def md_detail(array):
    return machine.succeed(f"mdadm --detail /dev/md/{array}")


def assert_mirrored(array, metadata):
    detail = md_detail(array)
    assert "Raid Level : raid1" in detail, detail
    assert f"Version : {metadata}" in detail, detail
    assert "Active Devices : 2" in detail, detail
    for member in ("/dev/vdb", "/dev/vdc"):
        assert member in detail, f"{array} is missing a member on {member}: {detail}"


machine.start()
machine.wait_for_unit("multi-user.target")

with subtest("mirror install completes"):
    machine.succeed(INSTALL, timeout=600)

with subtest("ESP is md RAID1 with metadata 1.0, mounted at /boot"):
    assert_mirrored("esp", "1.0")
    assert machine.succeed("findmnt -no SOURCE /mnt/boot").strip().startswith("/dev/md"), "ESP not on md"
    for part in ("/dev/vdb1", "/dev/vdc1"):
        ptype = machine.succeed(f"lsblk -no PARTTYPE {part}").strip()
        assert ptype == ESP_TYPE, f"{part} has type {ptype}, not an ESP"

with subtest("btrfs system is on md RAID1 and holds the blank snapshot"):
    assert_mirrored("system", "1.2")
    out = machine.succeed("btrfs filesystem show /mnt")
    assert out.count("devid") == 1 and "/dev/md" in out, out
    subvols = machine.succeed("btrfs subvolume list /mnt")
    for sv in ["@root", "@root-blank", "@nix", "@persist", "@log"]:
        assert sv in subvols, f"missing subvolume {sv}: {subvols}"

with subtest("every disk's remainder is a PV in VG expanse"):
    pvs = machine.succeed("pvs --noheadings -o pv_name,vg_name")
    for part in ("/dev/vdb3", "/dev/vdc3"):
        assert part in pvs and "expanse" in pvs, pvs

with subtest("a forced re-run formats fresh filesystems on the recreated arrays"):
    # A recreated md array exposes the old array's data; the reinstall must not reuse it.
    old = {fs: machine.succeed(f"findmnt -no UUID {fs}").strip() for fs in ("/mnt", "/mnt/boot")}
    machine.succeed(INSTALL, timeout=600)
    assert_mirrored("esp", "1.0")
    for fs, uuid in old.items():
        new = machine.succeed(f"findmnt -no UUID {fs}").strip()
        assert new != uuid, f"{fs} kept filesystem {uuid} across a forced reinstall"
    assert "@root-blank" in machine.succeed("btrfs subvolume list /mnt")
