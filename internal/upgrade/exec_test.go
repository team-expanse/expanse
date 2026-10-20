package upgrade

import (
	"reflect"
	"testing"
)

func TestBuildArgsTrustTheReleaseCacheOnlyWithItsKey(t *testing.T) {
	got := buildArgs("/etc/nixos/expanse/nix/node-system.nix", "file:///var/cache/c", "expanse-1:abc")
	want := []string{
		"/etc/nixos/expanse/nix/node-system.nix", "-A", "config.system.build.toplevel", "--no-out-link",
		"--option", "extra-substituters", "file:///var/cache/c",
		"--option", "extra-trusted-public-keys", "expanse-1:abc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if got := buildArgs("/f.nix", "", "expanse-1:abc"); len(got) != 4 {
		t.Errorf("without a cache: %v", got)
	}
}
