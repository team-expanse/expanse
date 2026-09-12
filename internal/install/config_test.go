package install

import (
	"strings"
	"testing"
)

const validYAML = `
version: 1
hostname: expanse-n1
disks:
  layout: auto
  devices: [/dev/sda]
  force: false
network:
  interface: auto
  mode: dhcp
cluster:
  mode: none
ssh:
  authorized_keys:
    - ssh-ed25519 AAAAFAKEKEY
timezone: UTC
`

func TestParseConfigValid(t *testing.T) {
	c, err := ParseConfig([]byte(validYAML))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if c.Version != 1 {
		t.Errorf("version = %d", c.Version)
	}
	if c.Hostname != "expanse-n1" {
		t.Errorf("hostname = %q", c.Hostname)
	}
	if c.Disks.Layout != LayoutAuto {
		t.Errorf("layout = %q", c.Disks.Layout)
	}
	if len(c.SSH.AuthorizedKeys) != 1 {
		t.Errorf("authorized_keys = %v", c.SSH.AuthorizedKeys)
	}
}

func TestParseConfigRejectsUnknownVersion(t *testing.T) {
	if _, err := ParseConfig([]byte("version: 2\n")); err == nil {
		t.Error("expected error for unknown version")
	}
}

func TestParseConfigRejectsStaticWithoutAddress(t *testing.T) {
	yaml := `
version: 1
network:
  mode: static
`
	_, err := ParseConfig([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "address") {
		t.Errorf("expected address error, got %v", err)
	}
}

func TestParseConfigRejectsBadLayout(t *testing.T) {
	yaml := `
version: 1
disks:
  layout: jbod
`
	if _, err := ParseConfig([]byte(yaml)); err == nil {
		t.Error("expected error for unknown layout")
	}
}

func TestResolvedLayout(t *testing.T) {
	cases := []struct {
		layout  DiskLayout
		disks   int
		want    DiskLayout
		wantErr bool
	}{
		{LayoutAuto, 1, LayoutSingle, false},
		{LayoutAuto, 2, LayoutMirror, false},
		{LayoutAuto, 3, LayoutRaidz1, false},
		{LayoutSingle, 2, "", true},
		{LayoutMirror, 1, "", true},
		{LayoutRaidz1, 2, "", true},
	}
	for _, tc := range cases {
		c := DefaultConfig()
		c.Disks.Layout = tc.layout
		got, err := c.ResolvedLayout(tc.disks)
		if tc.wantErr {
			if err == nil {
				t.Errorf("layout %s with %d disks: expected error", tc.layout, tc.disks)
			}
			continue
		}
		if err != nil {
			t.Errorf("layout %s with %d disks: %v", tc.layout, tc.disks, err)
		}
		if got != tc.want {
			t.Errorf("layout %s with %d disks = %s, want %s", tc.layout, tc.disks, got, tc.want)
		}
	}
}

func TestConfigRoundTrip(t *testing.T) {
	c, err := ParseConfig([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.String(), "expanse-n1") {
		t.Errorf("summary missing hostname:\n%s", c.String())
	}
}
