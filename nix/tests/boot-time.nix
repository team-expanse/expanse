# Boot time: 5 boots, median < 90s, none > 120s. Records slowest units.
{ self }:
{ pkgs, lib, ... }:
{
  name = "expanse-boot-time";

  nodes.machine = { config, pkgs, ... }: {
    imports = [ self.nixosModules.expanse ./pool-init.nix ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.hostId = "01234567";

    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    virtualisation.emptyDiskImages = [ 20480 ];
  };

  testScript = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    times = []
    for i in range(5):
        # crash()+start() instead of reboot(): the framework's soft
        # reboot path powers the test VM off into S5.
        machine.crash()
        machine.start()
        machine.wait_for_unit("multi-user.target")
        out = machine.succeed("systemd-analyze time")
        # "Finished in 12.3s" -> seconds
        seconds = None
        for token in out.replace("\\n", " ").split():
            if token.endswith("s") and token[:-1].replace(".", "").isdigit():
                seconds = float(token[:-1])
        assert seconds is not None, f"could not parse boot time: {out}"
        times.append(seconds)

    times.sort()
    median = times[len(times) // 2]
    print(f"boot times: {times}")
    assert median < 90, f"median boot time {median}s >= 90s"
    assert times[-1] < 120, f"worst boot time {times[-1]}s >= 120s"

    with subtest("slowest units recorded for report"):
        machine.succeed("systemd-analyze blame | head -5 >&2")
  '';
}
