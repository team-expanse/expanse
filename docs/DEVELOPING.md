# Expanse — Developer Guide

## Getting a Dev Environment

```bash
nix develop          # drops you into a shell with go, golangci-lint, protoc, qemu
```

The devshell provides: `go`, `gopls`, `golangci-lint`, `gofumpt`, `delve`, `protobuf`, `protoc-gen-go`, `protoc-gen-go-grpc`, `qemu`, `nixos-rebuild`, `jq`, `just`, `make`, `git`.

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
2. Add it to `flake.nix` checks:
   ```nix
   checks.<area> = pkgs.nixosTest (import ./nix/tests/<name>.nix { inherit self; });
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
- See `PRODUCT_DESIGN.md` §16 for full conventions.