# Pando

How to run [Pando](https://github.com/trypando/pando) on an Expanse cluster, and what happens to it
when the node running it fails.

## 1. The design, in one paragraph

Pando is a self-hosted platform that builds and runs your team's apps as Docker containers, with its
own Postgres and a BuildKit builder beside it. It needs a Docker daemon, which Expanse nodes do not
run, so Pando is not a block of its own. Instead it runs inside a `vm/instance` block
([`VMS.md`](VMS.md)): a NixOS guest with Docker, Pando 0.3.0 and its two companion images baked in.
The guest's whole disk is the block's DRBD-replicated volume. When the node running it fails, the
guest cold-boots on a survivor with the same MAC address, and Pando, its database and every app it
hosts come back from that disk. Pando is AGPL-3.0; the license covers Pando, not the apps you host on
it.

## 2. Building the guest image

The flake builds a raw, BIOS-bootable disk image with the three container images inside, so the
first boot needs no registry:

```sh
nix build .#pando-guest-image      # result/nixos.img, about 7.4 GB, about 3.4 GB of it data
```

To add SSH keys, set Pando's variables, or give the guest a static address, build your own from the
`nixosModules.pando-guest` module and `nix/guests/pando/image.nix`:

```nix
pando-image = import "${expanse}/nix/guests/pando/image.nix" {
  inherit nixpkgs;
  system = "x86_64-linux";
  modules = [{
    expanse.pandoGuest.authorizedKeys = [ "ssh-ed25519 AAAA... you@laptop" ];
    expanse.pandoGuest.settings = {
      PANDO_ADMIN_PASSWORD = "a-long-first-password";
      PANDO_SERVER_EXTERNAL_URL = "https://pando.example.com";
    };
  }];
};
```

| Option | Default | Meaning |
|---|---|---|
| `expanse.pandoGuest.authorizedKeys` | none | SSH keys for `root`; password login is off |
| `expanse.pandoGuest.settings` | none | Variables for Pando's compose file; Pando's README lists them |

`settings` is copied to `/var/lib/pando-guest/pando.env` on the first boot only. To change a
setting later, edit that file in the guest and run `systemctl restart pando`. `PANDO_ADMIN_PASSWORD`
(10 characters or more) is read on Pando's first run only; left unset, the first person to open the
console creates the administrator. The guest takes its address by DHCP unless a module sets one.

## 3. Deploying

A block adopts an existing volume with its name, `blk-<namespace>-<block>-<storage>`. So create
the volume first, write the image onto it, then apply the block.

**1. Create the volume** and wait until every replica is `UpToDate`:

```sh
expanse ctl volume create blk-default-pando-disk --size 40Gi --replication 3
drbdadm status          # on any node; note the resource name, vol-...
```

Size it for the apps Pando will host: their images, builds and data all live on this disk. The
guest grows its root filesystem to fill the volume on boot.

**2. Write the image** on the node where the resource is `Primary`. The volume is new, so the
regions `conv=sparse` skips already read as zeros:

```sh
dd if=result/nixos.img of=/dev/drbdN bs=4M conv=sparse,fsync oflag=direct status=progress
```

**3. Apply the block.** Its storage entry must name the same volume (`disk`) and be raw:

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: pando
  namespace: default
spec:
  type: vm/instance
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 2000m
      memory: 4Gi
  storage:
    - name: disk
      size: 40Gi
      replication: 3
      mountPath: /mnt/pando-disk
      filesystem: none
  placement:
    requiredCapabilities: [kvm]
```

```sh
expanse ctl block apply -f pando.yaml
```

The block reaches RUNNING once the guest reaches `multi-user.target`, which waits for Pando's
containers to start. The first boot also loads the three images into Docker, so it takes longer.

## 4. Reaching Pando

The guest prints its console address on its serial console, which is the block's journal on the
node running it:

```sh
journalctl -u expanse-block-root@default-pando-0.service | grep pando-guest
# pando-guest: console at http://192.168.1.60:8080
```

Open that address in a browser. Apps Pando publishes by port get ports 9000–9019 on the same
address. The guest's address stays the same across failovers. Like every `vm/instance`, the guest
is not reachable from the node that is running it (`ARCHITECTURE.md` A34); use any other machine.

**Security.** Pando serves plain HTTP on port 8080. Put a TLS proxy in front of it and set
`PANDO_SERVER_EXTERNAL_URL`. The guest's firewall opens only ports 22, 8080 and 9000–9019.

## 5. Failover, from Pando's perspective

When the node running the guest dies, the block and its volume's DRBD primary move to a survivor and
the guest cold-boots there. This is a power loss for the guest: Postgres recovers from its write-ahead
log, Docker restarts every container with a `restart` policy, and in-flight requests and builds
are lost. Clients reconnect to the same address.

What survives is what reached the disk. Postgres syncs every commit, so Pando's database is safe.
Files written without `fsync` can be lost if the node dies within about 30 seconds of the write.
That window broke first boots in testing. The Postgres image writes `pg_hba.conf`, and Pando writes
its `secrets.key`, on first start without syncing either. A crash right after first start left an
empty key and a Postgres that refused Pando. So `pando.service` runs `sync` once the stack is
healthy, and the guest reports ready, which is what moves the block to RUNNING, only after that.

Measured in the `pando-guest` VM test (3 nested-virtualized nodes, 5 s agent period): the block and
its volume primary agreed on a survivor 51 s after the node was crashed, and Pando's console answered
again 90 s after the crash, the guest having cold-booted in between. A group created through
Pando's API just before the crash was still there.

## 6. What is not covered

- Live migration: a failover is always a cold boot ([`VMS.md`](VMS.md) §6).
- More than one Pando guest serving the same apps. Pando runs apps on its own host only.
- Upgrades of the guest or of Pando. Writing a newer image over the volume erases everything on it;
  move Pando's data into a new guest with Pando's own backups instead.
- Backups: a crash-consistent volume snapshot ([`BACKUP.md`](BACKUP.md)) captures the whole guest.
