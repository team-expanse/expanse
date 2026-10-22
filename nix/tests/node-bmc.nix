# A node with a BMC: the agent reads its LAN address through /dev/ipmi0 and the node page links
# it. OpenIPMI's ipmi_sim is the BMC (QEMU's built-in one has no LAN channel). The scenario is
# python/node_bmc.py.
{ self }:
{ pkgs, lib, ... }:
let
  shim = "n2 = n3 = n1\n";
  lancontrol = pkgs.writeShellScript "lancontrol" (builtins.readFile ./bmc/lancontrol.sh);
  lanConf = pkgs.writeText "lan.conf" ''
    name "expanse"
    set_working_mc 0x20
      startlan 1
        addr 127.0.0.1 9001
        priv_limit admin
        allowed_auths_admin none md5 straight
        guid a123456789abcdefa123456789abcdef
        lan_config_program "${lancontrol} eth9"
      endlan
      serial 15 127.0.0.1 9002 codec VM
      startnow false
  '';
  defs = ''
    IPMI_SIM = ["${pkgs.openipmi}/bin/ipmi_sim", "-n", "-c", "${lanConf}", "-f", "${./bmc/bmc.emu}"]
  '';
  lint = pkgs.runCommand "node-bmc-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg (shim + defs)}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/ui_forms_common.py} ${./python/node_bmc.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    sh -n ${./bmc/lancontrol.sh}
    touch $out
  '';
in
{
  name = "expanse-node-bmc";

  nodes.n1 = { ... }: {
    imports = [ self.nixosModules.expanse ./curl-cookie-jar.nix ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "00000001";
    expanse.hostname = "n1";
    expanse.agent.raftAdvertise = "192.168.1.1:7444";
    environment.systemPackages = with pkgs; [ curl ];
    virtualisation.memorySize = 1536;
    # The KCS interface to ipmi_sim, which the test script starts before the VM.
    virtualisation.qemu.options = [
      "-chardev socket,id=ipmi0,host=127.0.0.1,port=9002,reconnect-ms=1000"
      "-device ipmi-bmc-extern,id=bmc0,chardev=ipmi0"
      "-device isa-ipmi-kcs,bmc=bmc0,irq=5"
    ];
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${shim}
    ${defs}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/single_node_common.py}
    ${builtins.readFile ./python/ui_forms_common.py}
    ${builtins.readFile ./python/node_bmc.py}
  '';
}
