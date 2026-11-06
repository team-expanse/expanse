# A VM's serial console from the CLI and the web UI, on its own node and from another:
# `expanse ctl vm console` and the console page's WebSocket both reach the guest's ttyS0.
# vm-instance.nix's cluster and guest, whose netboot image logs in on its serial console.
{ self }:
import ./vm-instance.nix {
  inherit self;
  name = "vm-console";
  script = ./python/vm_console.py;
  nodeModule = { pkgs, ... }: {
    environment.systemPackages = [ (pkgs.python3.withPackages (ps: [ ps.websockets ])) ];
    environment.etc."expanse-test/console_ws.py".source = ./python/console_ws.py;
  };
}
