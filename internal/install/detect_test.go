package install

import (
	"strings"
	"testing"
)

// fixtureLsblk mimics `lsblk -J` on a machine with a 500G data disk, a
// 20G blank disk, the installer USB stick (removable), and a CD-ROM.
const fixtureLsblk = `{
  "blockdevices": [
    {"name":"/dev/sda","path":"/dev/sda","size":"500G","model":"Samsung SSD 860","type":"disk","rm":false,
     "mountpoint":null,"fstype":null,
     "children":[{"name":"/dev/sda1","size":"1G","type":"part","mountpoint":null,"fstype":"ntfs"}]},
    {"name":"/dev/sdb","path":"/dev/sdb","size":"20G","model":"QEMU HARDDISK","type":"disk","rm":false,
     "mountpoint":null,"fstype":null,"children":[]},
    {"name":"/dev/sdc","path":"/dev/sdc","size":"32G","model":"USB SanDisk","type":"disk","rm":true,
     "mountpoint":"/run/media/iso","fstype":"iso9660","children":[]},
    {"name":"/dev/sr0","path":"/dev/sr0","size":"1G","model":"","type":"rom","rm":true,
     "mountpoint":null,"fstype":null,"children":[]},
    {"name":"/dev/vda","path":"/dev/vda","size":"1.5T","model":"","type":"disk","rm":false,
     "mountpoint":null,"fstype":"ext4","children":[]}
  ]
}`

func loadFixtureDisks(t *testing.T) []Disk {
	t.Helper()
	disks, err := ParseLsblk([]byte(fixtureLsblk))
	if err != nil {
		t.Fatalf("ParseLsblk: %v", err)
	}
	return disks
}

func TestParseLsblkFiltersRemovableAndRom(t *testing.T) {
	disks := loadFixtureDisks(t)
	if len(disks) != 3 {
		t.Fatalf("got %d disks, want 3 (removable + rom filtered)", len(disks))
	}
	for _, d := range disks {
		if d.RM {
			t.Errorf("removable disk %s not filtered", d.Path)
		}
	}
}

func TestDiskNonEmpty(t *testing.T) {
	disks := loadFixtureDisks(t)
	byPath := map[string]Disk{}
	for _, d := range disks {
		byPath[d.Path] = d
	}
	if !byPath["/dev/sda"].NonEmpty() {
		t.Error("/dev/sda has an ntfs child, should be non-empty")
	}
	if !byPath["/dev/vda"].NonEmpty() {
		t.Error("/dev/vda has a filesystem directly, should be non-empty")
	}
	if byPath["/dev/sdb"].NonEmpty() {
		t.Error("/dev/sdb is blank, should be empty")
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"500G", 500 * 1024 * 1024 * 1024},
		{"20G", 20 * 1024 * 1024 * 1024},
		{"1.5T", 3 * 1024 * 1024 * 1024 * 512},
		{"512B", 512},
	}
	for _, tc := range cases {
		got, err := parseSize(tc.in)
		if err != nil {
			t.Errorf("parseSize(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSelectDisksAutoLayout(t *testing.T) {
	disks := loadFixtureDisks(t)
	cfg := DefaultConfig()
	cfg.Disks.Devices = []string{"/dev/sdb"}
	sel, layout, err := SelectDisks(cfg, disks, false)
	if err != nil {
		t.Fatalf("SelectDisks: %v", err)
	}
	if len(sel) != 1 || sel[0].Path != "/dev/sdb" {
		t.Fatalf("selected = %v", sel)
	}
	if layout != LayoutSingle {
		t.Errorf("layout = %s", layout)
	}
}

func TestSelectDisksRefusesNonEmptyWithoutForce(t *testing.T) {
	disks := loadFixtureDisks(t)
	cfg := DefaultConfig()
	cfg.Disks.Devices = []string{"/dev/sda"}
	_, _, err := SelectDisks(cfg, disks, false)
	if err == nil {
		t.Fatal("expected refusal for non-empty disk without force")
	}
	for _, want := range []string{"non-empty", "/dev/sda"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	// With force it succeeds.
	if _, _, err := SelectDisks(cfg, disks, true); err != nil {
		t.Errorf("SelectDisks with force: %v", err)
	}
}

func TestSelectDisksRejectsUnknownDevice(t *testing.T) {
	disks := loadFixtureDisks(t)
	cfg := DefaultConfig()
	cfg.Disks.Devices = []string{"/dev/sdz"}
	if _, _, err := SelectDisks(cfg, disks, true); err == nil {
		t.Fatal("expected error for nonexistent device")
	}
}

func TestSelectDisksAutoDetectUsesAllDisks(t *testing.T) {
	disks := loadFixtureDisks(t)
	cfg := DefaultConfig()                            // devices empty
	sel, layout, err := SelectDisks(cfg, disks, true) // force: TUI/confirm gate handles refusal
	if err != nil {
		t.Fatalf("SelectDisks: %v", err)
	}
	if len(sel) != 3 {
		t.Errorf("auto-detect selected %d disks, want 3", len(sel))
	}
	if layout != LayoutRaidz1 {
		t.Errorf("layout = %s, want raidz1", layout)
	}
}

func TestRunPreflight(t *testing.T) {
	disks := loadFixtureDisks(t)
	pf, err := RunPreflight(disks)
	if err != nil {
		t.Fatalf("RunPreflight: %v", err)
	}
	if !pf.DiskOK {
		t.Error("expected at least one disk >= 20G")
	}
	if _, err := RunPreflight(nil); err == nil {
		t.Error("expected error with no disks")
	}
}
