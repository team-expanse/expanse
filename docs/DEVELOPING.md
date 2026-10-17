# Expanse — Developer Guide

## Getting a Dev Environment

```bash
nix develop          # drops you into a shell with go, golangci-lint, protoc, qemu
```

The devshell provides: `go`, `gopls`, `golangci-lint`, `gofumpt`, `delve`, `protobuf`, `protoc-gen-go`, `protoc-gen-go-grpc`, `qemu`, `nixos-rebuild`, `jq`, `just`, `make`, `git`.

### A dev environment in an Expanse VM

Expanse can host its own development machine: a NixOS guest in a `vm/instance` block with nested
KVM, so `nix build .#checks...` runs the VM tests inside it. The guest is the
`nixosModules.dev-guest` module: nix with flakes and the `kvm`/`nixos-test` system features, git,
github-cli, go, python3, qemu, vim and an SSH-only login user in `wheel` and `kvm`.

**1. Build the image** with your keys, from a flake of your own:

```nix
{
  inputs.expanse.url = "github:team-expanse/expanse";
  outputs = { expanse, ... }: {
    packages.x86_64-linux.dev-image = expanse.lib.devGuestImage {
      modules = [ ({ pkgs, ... }: {
        networking.hostName = "dev";
        time.timeZone = "America/Denver";
        expanse.devGuest.user = "you";
        expanse.devGuest.authorizedKeys = [ "ssh-ed25519 AAAA... you@laptop" ];
        # Unfree tools go here, with your own consent to their licenses.
        nixpkgs.config.allowUnfree = true;
        expanse.devGuest.extraPackages = [ pkgs.claude-code ];
      }) ];
    };
  };
}
```

`nix build .#dev-image` gives `result/nixos.img`. `nix build .#dev-guest-image` in this repo builds
the same guest with no keys, which nobody can log in to.

| Option | Default | Meaning |
|---|---|---|
| `expanse.devGuest.user` | `dev` | The login user; sudo needs no password |
| `expanse.devGuest.authorizedKeys` | none | SSH keys for that user; password login is off |
| `expanse.devGuest.extraPackages` | none | Packages beside the default toolchain |

**2. Create the volume and write the image** on the node where the volume is `Primary`. The guest
grows its root filesystem to fill the volume on boot, so size it for the nix store and your work:

```sh
expanse ctl volume create blk-default-dev-disk --size 300Gi --replication 1
drbdadm status          # note the resource and its /dev/drbdN
dd if=nixos.img of=/dev/drbdN bs=4M conv=sparse,fsync oflag=direct status=progress
```

**3. Apply the block.** `cpuModel: host` passes `vmx` through ([`VMS.md`](VMS.md)); `mac` pins an
address you can reserve in your router's DHCP before the first boot:

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: dev
  namespace: default
spec:
  type: vm/instance
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 16000m
      memory: 64Gi
  storage:
    - name: disk
      size: 300Gi
      replication: 1
      mountPath: /mnt/dev-disk
      filesystem: none
  placement:
    requiredCapabilities: [kvm]
  config:
    cpuModel: host
    mac: "02:00:00:00:de:01"
```

Replace the `mac` with any locally administered address (second hex digit 2, 6, A or E). Once the
block is RUNNING, SSH in from any machine other than the node running it (`VMS.md`; a host cannot
reach its own macvtap guest). Afterwards, manage the guest with `nixos-rebuild` inside it from a
flake that imports `expanse.nixosModules.dev-guest`. `nix/tests/dev-guest.nix` tests this recipe.

**4. Back it up.** A volume with `replication: 1` lives on one disk set, and the nix store can be
rebuilt but your work cannot. Turn on restic in the guest's modules:

```nix
expanse.devGuest.backup = {
  enable = true;
  repository = "rest:http://nas.lan:8000/dev";   # any restic repository: sftp:, s3:, rest:
  passwordFile = "/var/lib/restic/password";     # a path in the guest, never the nix store
};
```

| Option | Default | Meaning |
|---|---|---|
| `backup.paths` | `/home`, `/etc/nixos` | Every user's repos, `~/.ssh` and dotfiles, and the guest's config |
| `backup.exclude` | `/home/*/.cache` | Left out |
| `backup.schedule` | `daily` | `OnCalendar`; a run missed while the guest was off starts at boot |
| `backup.environmentFile` | none | Credentials for S3 or a REST server login |

Write the password file (`sudo install -m 0400 /dev/stdin /var/lib/restic/password`) and keep a
copy off the guest: without it the backups cannot be read. The first run creates the repository.
The nix store, `/var` and anything outside `/home` are not backed up; rebuild them from the
guest's flake. Run the restore drill once after setting it up:

```sh
sudo systemctl start restic-backups-dev.service    # back up now
sudo restic-dev snapshots
sudo restic-dev restore latest --target / --include /home/you/src/project
```

`restic-dev` is restic with the repository and password already set. To rebuild a lost guest,
write a fresh image as in steps 1–3, then `sudo restic-dev restore latest --target /` restores
everything.

## Building and Running

```bash
make build           # builds to bin/expanse
./bin/expanse version   # prints version info
./bin/expanse --help    # shows help
```

With Nix:
```bash
nix build .#expanse -L
result/bin/expanse version
```

## Running Tests

```bash
make test            # unit tests with coverage, race detection
make lint            # golangci-lint
make perf            # performance budget checks
make vm-test         # NixOS VM smoke test (requires KVM)
```

With Nix:
```bash
nix flake check      # all checks: lint + unit + vm-test
```

## Adding a New Subcommand

1. Create `cmd/expanse/cmd_<name>.go` with a `new<Name>Cmd() *cobra.Command` function.
2. Wire it in `cmd/expanse/main.go`: add `rc.cmd.AddCommand(new<Name>Cmd())`.
3. Add help text and flags as needed.
4. Test: `./bin/expanse <name> --help` should show your command.

## Adding a New NixOS VM Test

1. Create `nix/tests/<name>.nix` following the pattern in `nix/tests/smoke.nix`.
2. Add it to `flake.nix` checks, using its own `mkTest` helper (every existing
   check follows this form, not a bare `pkgs.nixosTest` call):
   ```nix
   checks.<area> = mkTest "<area>" ./nix/tests/<name>.nix;
   ```

## Adding a Performance Budget

1. Append to `test/perf/budgets.yaml` under the `budgets:` key.
2. Add measurement logic in `test/perf/budget.go`.
3. Never remove an existing budget — only tighten constraints.

## Code Conventions

- One `package` per directory.
- No `fmt.Println` outside `cmd/` output formatting; use `log/slog` for logging.
- Every log line from a subsystem carries `component=<name>`.
- Errors always logged with key `err`.
- `//nolint` only with an explanation comment.