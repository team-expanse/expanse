package install

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Disk describes a block device as reported by lsblk.
type Disk struct {
	Name       string     `json:"name"`
	Path       string     `json:"path"`
	Size       string     `json:"size"`
	SizeBytes  int64      `json:"-"`
	Model      string     `json:"model"`
	Type       string     `json:"type"` // "disk" vs "part"/"rom"
	RM         bool       `json:"rm"`   // removable
	Mountpoint string     `json:"mountpoint"`
	FSType     string     `json:"fstype"`
	Children   []DiskPart `json:"children"`
}

// DiskPart is a child partition of a disk.
type DiskPart struct {
	Name       string `json:"name"`
	Size       string `json:"size"`
	Type       string `json:"type"`
	Mountpoint string `json:"mountpoint"`
	FSType     string `json:"fstype"`
}

type lsblkOutput struct {
	Blockdevices []Disk `json:"blockdevices"`
}

// ParseLsblk parses lsblk -J -o ... JSON output.
func ParseLsblk(data []byte) ([]Disk, error) {
	var out lsblkOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse lsblk output: %w", err)
	}
	var disks []Disk
	for _, d := range out.Blockdevices {
		if d.Type == "disk" && !d.RM {
			disks = append(disks, d)
		}
	}
	return disks, nil
}

// DetectDisks runs lsblk and returns candidate target disks, excluding the
// installer medium (removable devices) and loop devices.
func DetectDisks() ([]Disk, error) {
	cmd := exec.Command("lsblk", "-J", "-p", "-o",
		"NAME,PATH,SIZE,MODEL,TYPE,RM,MOUNTPOINT,FSTYPE")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	return ParseLsblk(out)
}

// NonEmpty reports whether the disk holds any filesystem or partition.
func (d Disk) NonEmpty() bool {
	if d.FSType != "" || d.Mountpoint != "" {
		return true
	}
	for _, c := range d.Children {
		if c.FSType != "" || c.Mountpoint != "" {
			return true
		}
	}
	return false
}

// SizeBytes parses the lsblk human size ("500G", "1.5T") into bytes.
func (d Disk) SizeBytesOf() (int64, error) {
	return parseSize(d.Size)
}

// parseSize parses lsblk sizes like "512B", "20G", "1.5T".
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	unit := s[len(s)-1]
	switch unit {
	case 'K':
		mult, unit = 1024, '0'
	case 'M':
		mult, unit = 1024*1024, '0'
	case 'G':
		mult, unit = 1024*1024*1024, '0'
	case 'T':
		mult, unit = 1024*1024*1024*1024, '0'
	case 'P':
		mult, unit = 1024*1024*1024*1024*1024, '0'
	case 'B':
		unit = '0'
	}
	numPart := s[:len(s)-1]
	if unit != '0' {
		numPart = s
	}
	var n float64
	if _, err := fmt.Sscanf(numPart, "%g", &n); err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}
	return int64(n * float64(mult)), nil
}

// Preflight holds detected hardware facts.
type Preflight struct {
	Arch         string
	MemBytes     int64
	DiskOK       bool
	MinDiskBytes int64
	Warnings     []string
}

// MinRAM is the minimum supported memory.
const MinRAM = 2 * 1024 * 1024 * 1024

// MinDisk is the minimum supported target disk size.
const MinDisk = 20 * 1024 * 1024 * 1024

// RunPreflight performs pre-install checks. Warnings are advisory; errors
// block the install.
func RunPreflight(disks []Disk) (*Preflight, error) {
	p := &Preflight{Arch: "x86_64", MinDiskBytes: MinDisk}
	// RAM check via /proc/meminfo.
	if data, err := readFileString("/proc/meminfo"); err == nil {
		var kb int64
		if _, err := fmt.Sscanf(strings.Split(data, "\n")[0], "MemTotal: %d kB", &kb); err == nil && kb > 0 {
			p.MemBytes = kb * 1024
		}
	}
	if p.MemBytes > 0 && p.MemBytes < MinRAM-100*1024*1024 {
		return nil, fmt.Errorf("insufficient RAM: %d MiB (need ~2048 MiB)", p.MemBytes/1024/1024)
	}
	for _, d := range disks {
		sz, err := d.SizeBytesOf()
		if err != nil {
			p.Warnings = append(p.Warnings, fmt.Sprintf("cannot determine size of %s: %v", d.Path, err))
			continue
		}
		if sz < MinDisk {
			p.Warnings = append(p.Warnings, fmt.Sprintf("disk %s is %s (< 20G)", d.Path, d.Size))
		} else {
			p.DiskOK = true
		}
	}
	if len(disks) == 0 {
		return nil, fmt.Errorf("no candidate disks found")
	}
	return p, nil
}

// SelectDisks resolves which disks and layout to use for the install.
// It validates device existence, minimum counts per layout, and the
// refusal to wipe non-empty disks without force.
func SelectDisks(cfg *Config, disks []Disk, force bool) ([]Disk, DiskLayout, error) {
	byPath := map[string]Disk{}
	for _, d := range disks {
		byPath[d.Path] = d
		byPath[d.Name] = d
	}
	var selected []Disk
	if len(cfg.Disks.Devices) == 0 {
		// Auto-detect: use every candidate disk.
		selected = disks
	} else {
		for _, dev := range cfg.Disks.Devices {
			d, ok := byPath[dev]
			if !ok {
				return nil, "", fmt.Errorf("device %s: not found or not a candidate disk", dev)
			}
			selected = append(selected, d)
		}
	}
	layout, err := cfg.ResolvedLayout(len(selected))
	if err != nil {
		return nil, "", err
	}
	if !force && !cfg.Disks.Force {
		for _, d := range selected {
			if d.NonEmpty() {
				return nil, "", fmt.Errorf(
					"refusing to wipe non-empty disk %s (%s %s, contains: %s); pass --force to override",
					d.Path, d.Model, d.Size, describeContents(d))
			}
		}
	}
	return selected, layout, nil
}

// describeContents summarizes what is on a disk for the confirmation prompt.
func describeContents(d Disk) string {
	var parts []string
	for _, c := range d.Children {
		if c.FSType != "" {
			parts = append(parts, c.FSType+" partition "+c.Name)
		} else {
			parts = append(parts, "partition "+c.Name)
		}
	}
	if len(parts) == 0 {
		if d.FSType != "" {
			return d.FSType + " filesystem"
		}
		return "no partitions"
	}
	return strings.Join(parts, ", ")
}

// readFileString reads a file into a string, best-effort helper.
func readFileString(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}
