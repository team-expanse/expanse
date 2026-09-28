"""install-tui: the installer TUI on tty1, driven by keystrokes, installs onto a blank disk.

The VM's own root is /dev/vda, so the target is the second disk listed, /dev/vdb. The install
runs with --skip-system-install (install-tui.nix), so it stops before nixos-install.
"""

SSH_KEY = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItuitest tui@test"
HOSTNAME = "tui-node"


def screen(text, timeout=120):
    machine.wait_until_tty_matches("1", text, timeout=timeout)


def dump_on_failure():
    print(machine.execute("cat /dev/vcs1 | fold -w 80")[1])
    print(machine.execute("cat /tmp/expanse-install.log")[1])


machine.start()
machine.wait_for_unit("multi-user.target")

try:
    with subtest("the installer starts on tty1 and ENTER leaves the welcome screen"):
        screen("EXPANSE INSTALLER")
        screen("Step 1 of 6")
        screen("/dev/vdb")
        machine.send_key("ret")
        screen("Step 2 of 6")
        screen("Pick the target disk")

    with subtest("a disk must be picked, then the second one is"):
        machine.send_key("ret")
        screen("select at least one disk")
        machine.send_key("down")
        machine.send_chars(" ")
        screen(r"\[x\] /dev/vdb")
        screen("1 disk selected: single layout")
        machine.send_key("ret")
        screen("Step 3 of 6")
        screen("Hostname")

    with subtest("hostname and SSH key are typed in, and the review shows them"):
        machine.send_chars(HOSTNAME)
        machine.send_key("ret")
        screen("Step 4 of 6")
        screen("Public key")
        machine.send_chars(SSH_KEY)
        machine.send_key("ret")
        screen("Step 5 of 6")
        screen("WILL DESTROY all data on")
        screen(HOSTNAME)
        screen("/dev/vdb")
        screen("Type INSTALL")

    with subtest("typing INSTALL runs the install through to the done screen"):
        machine.send_chars("INSTALL")
        screen("INSTALL (COMPLETE|FAILED)", timeout=600)
        screen("INSTALL COMPLETE", timeout=5)
        screen(r"Node ID +[0-9a-f]{8}-")
        screen(r"https://[0-9.]+:8443")
        screen("expanse cluster init --expect 1")
        screen("journalctl -u expansed")
        machine.screenshot("installer-done")

    with subtest("the install did what the screens said"):
        out = machine.succeed("btrfs subvolume list /mnt")
        for sv in ["@root", "@root-blank", "@nix", "@persist", "@log"]:
            assert sv in out, f"missing subvolume {sv}: {out}"
        conf = machine.succeed("cat /mnt/persist/etc/nixos/configuration.nix")
        assert HOSTNAME in conf and SSH_KEY in conf, conf
        machine.succeed("grep -q availableKernelModules /mnt/persist/etc/nixos/hardware-configuration.nix")
        install_log = machine.succeed("cat /tmp/expanse-install.log")
        assert "==> partition" in install_log and "<== verify done" in install_log, install_log

    with subtest("a key on the done screen leaves the installer for a shell"):
        machine.send_key("ret")
        screen("Installer exited")
except Exception:
    dump_on_failure()
    raise

print("INSTALL-TUI PASSED: welcome -> disks -> hostname -> key -> INSTALL -> done, over tty1")
