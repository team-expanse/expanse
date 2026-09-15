# §6 net-mesh: 3-node cluster forms a full WireGuard mesh on exp0 —
# 2 peers per node, full overlay ping matrix, 1400-byte DF pings
# (the §4.1 MTU diagnostic), and a 4th-node join reforms the mesh to
# 3 peers with a 4×4 matrix within 30 s (G5.2).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-net-mesh";

  nodes = {
    n1 = { ... }: {
      imports = [ self.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000001";
      expanse.hostname = "n1";
      virtualisation.memorySize = 1536;
      networking.firewall.allowedUDPPorts = [ 51820 ];
      environment.systemPackages = [ pkgs.wireguard-tools ];
    };
    n2 = { ... }: {
      imports = [ self.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000002";
      expanse.hostname = "n2";
      virtualisation.memorySize = 1536;
      networking.firewall.allowedUDPPorts = [ 51820 ];
      environment.systemPackages = [ pkgs.wireguard-tools ];
    };
    n3 = { ... }: {
      imports = [ self.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000003";
      expanse.hostname = "n3";
      virtualisation.memorySize = 1536;
      networking.firewall.allowedUDPPorts = [ 51820 ];
      environment.systemPackages = [ pkgs.wireguard-tools ];
    };
    # The 4th node joins mid-test (mesh reform, G5.2).
    n4 = { ... }: {
      imports = [ self.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000004";
      expanse.hostname = "n4";
      virtualisation.memorySize = 1536;
      networking.firewall.allowedUDPPorts = [ 51820 ];
      environment.systemPackages = [ pkgs.wireguard-tools ];
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./client-common.py}

    # 4 uses: n2, n3 at formation + 1 left over for n4's mid-test join.
    form("netmesh", uses=4)

    with subtest("exp0 exists with MTU 1420 on every node"):
        for m in [n1, n2, n3]:
            wg = m.succeed("wg show exp0 2>&1")
            mtu = m.succeed("ip link show exp0 | grep -o 'mtu [0-9]*'")
            assert "mtu 1420" in mtu, f"{m.name}: exp0 MTU wrong: {mtu}"

    with subtest("full mesh: 2 peers on each of the 3 nodes (<= 60 s)"):
        for m in [n1, n2, n3]:
            wait_wg_peers(m, 2, 60)

    with subtest("each node holds its own overlay address 10.42.N.1"):
        for m in [n1, n2, n3]:
            ip = m.succeed("ip -4 -o addr show exp0")
            assert "10.42." in ip, f"{m.name}: no overlay address: {ip}"

    with subtest("3x3 overlay ping matrix (excluding self)"):
        ping_matrix([n1, n2, n3])

    with subtest("DF ping at the MTU limit (1400-byte packet, §4.1 diagnostic)"):
        ping_matrix([n1, n2, n3], ("df", DF_SIZE))

    with subtest("4th node joins; mesh reforms to 3 peers + 4x4 matrix (<= 30 s, G5.2)"):
        # The formation token persists on n2 (join_and_start writes it)
        # with one use left.
        token = n2.succeed("cat /root/join-token").strip()
        # n4's daemon auto-started at boot un-enrolled; stop it so the
        # post-join start actually picks up the cluster store.
        n4.succeed("systemctl stop expansed.service")
        join_and_start(n4, token, "voter")
        for m in [n1, n2, n3, n4]:
            wait_wg_peers(m, 3, 30)
        ping_matrix([n1, n2, n3, n4])
        ping_matrix([n1, n2, n3, n4], ("df", DF_SIZE))
  '';
}
