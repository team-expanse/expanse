package install

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// flakePlaceholder stands in for the installer's flake path; the node-config-eval check substitutes the real one.
const flakePlaceholder = "@flake@"

// TestWriteConfigurationGolden pins the configuration.nix nixos-install builds; node-config-eval evaluates these files.
func TestWriteConfigurationGolden(t *testing.T) {
	for _, tc := range []struct {
		layout DiskLayout
		disks  []string
	}{
		{LayoutSingle, []string{"/dev/vda"}},
		{LayoutMirror, []string{"/dev/vda", "/dev/vdb"}},
	} {
		cfg := DefaultConfig()
		cfg.Hostname = "golden-node"
		cfg.Disks.Devices = tc.disks
		cfg.SSH.AuthorizedKeys = []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIgolden golden@test"}
		rc := &RunContext{Config: cfg, Layout: tc.layout, TargetFlake: flakePlaceholder}

		got := filepath.Join(t.TempDir(), "configuration.nix")
		if err := writeConfiguration(rc, got); err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("..", "..", "test", "fixtures", "install", "configuration-"+string(tc.layout)+".nix")
		gotBytes, _ := os.ReadFile(got)
		if *update {
			if err := os.WriteFile(golden, gotBytes, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%s: %v (run with -update)", golden, err)
		}
		if string(gotBytes) != string(want) {
			t.Errorf("%s layout: got\n%s\nwant\n%s", tc.layout, gotBytes, want)
		}
	}
}
