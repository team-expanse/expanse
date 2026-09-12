// Package inventory collects hardware/system inventory from sysfs and
// /proc. Rule: never parse human-readable command output when a sysfs or
// JSON source exists.
package inventory

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Roots points the collector at the filesystem (overridable in tests).
type Roots struct {
	Proc string // default /proc
	Sys  string // default /sys
}

func (r Roots) proc() string {
	if r.Proc == "" {
		return "/proc"
	}
	return r.Proc
}

func (r Roots) sys() string {
	if r.Sys == "" {
		return "/sys"
	}
	return r.Sys
}

// Inventory is the collected node inventory (serialized to JSON for the
// API; field names are the stable contract).
type Inventory struct {
	NodeID         string     `json:"node_id"`
	Hostname       string     `json:"hostname"`
	OS             OSInfo     `json:"os"`
	CPU            CPUInfo    `json:"cpu"`
	Memory         MemoryInfo `json:"memory"`
	Disks          []DiskInfo `json:"disks"`
	Network        []NICInfo  `json:"nics"`
	GPUs           []GPUInfo  `json:"gpus"`
	Virtualization string     `json:"virtualization"`
	TPM            *TPMInfo   `json:"tpm,omitempty"`
	Capabilities   []string   `json:"capabilities"`
}

type OSInfo struct {
	Name              string `json:"name"`
	Version           string `json:"version"`
	Kernel            string `json:"kernel"`
	SystemClosurePath string `json:"system_closure_path"`
}

type CPUInfo struct {
	Model   string   `json:"model"`
	Cores   int      `json:"cores"`
	Threads int      `json:"threads"`
	MHz     float64  `json:"mhz"`
	Flags   []string `json:"flags"`
}

type MemoryInfo struct {
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"`
	SwapTotal uint64 `json:"swap_total"`
}

type DiskInfo struct {
	Path       string `json:"path"`
	Model      string `json:"model"`
	Serial     string `json:"serial"`
	Size       uint64 `json:"size"`
	Rotational bool   `json:"rotational"`
}

type NICInfo struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac"`
	SpeedMbps int      `json:"speed_mbps"`
	Addresses []string `json:"addresses"`
	Up        bool     `json:"up"`
}

type GPUInfo struct {
	Vendor string `json:"vendor"`
	Model  string `json:"model"`
	VRAM   int64  `json:"vram_mib"`
	Driver string `json:"driver"`
	PCIID  string `json:"pci_id"`
}

type TPMInfo struct {
	Present bool   `json:"present"`
	Version string `json:"version"`
}

// Collector gathers inventory.
type Collector struct {
	Roots Roots
}

// New creates a collector with default roots.
func New() *Collector { return &Collector{} }

// Collect gathers the full inventory. Sources that are absent degrade to
// zero values; collection never fails because one subsystem is missing.
func (c *Collector) Collect(nodeID string) (*Inventory, error) {
	r := c.Roots
	inv := &Inventory{
		NodeID:         nodeID,
		Hostname:       firstLine(r.proc() + "/hostname"),
		OS:             c.osInfo(r),
		CPU:            c.cpuInfo(r),
		Memory:         c.memInfo(r),
		Disks:          c.disks(r),
		Network:        c.nics(r),
		GPUs:           c.gpus(r),
		Virtualization: c.virtualization(r),
		TPM:            c.tpm(r),
	}
	inv.Capabilities = capabilities(inv)
	return inv, nil
}

func firstLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if i := indexByte(data, '\n'); i >= 0 {
		return string(data[:i])
	}
	return string(data)
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func (c *Collector) osInfo(r Roots) OSInfo {
	info := OSInfo{}
	// /etc/os-release (structured KEY=VALUE).
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			v = strings.Trim(v, `"`)
			switch k {
			case "NAME":
				info.Name = v
			case "VERSION":
				info.Version = v
			}
		}
	}
	info.Kernel = firstLine(r.proc() + "/sys/kernel/osrelease")
	// /run/current-system symlink gives the NixOS closure path.
	if p, err := os.Readlink("/run/current-system"); err == nil {
		info.SystemClosurePath = p
	}
	return info
}

func (c *Collector) cpuInfo(r Roots) CPUInfo {
	var cpu CPUInfo
	f, err := os.Open(r.proc() + "/cpuinfo")
	if err != nil {
		return cpu
	}
	defer f.Close()
	cores, threads := map[string]bool{}, map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "model name":
			if cpu.Model == "" {
				cpu.Model = v
			}
		case "processor":
			threads[v] = true
		case "core id":
			// counted per physical package below via (core id, physical id)
		case "cpu MHz":
			if mhz, err := strconv.ParseFloat(v, 64); err == nil && cpu.MHz == 0 {
				cpu.MHz = mhz
			}
		case "flags":
			if len(cpu.Flags) == 0 {
				cpu.Flags = strings.Fields(v)
			}
		}
	}
	cpu.Threads = len(threads)
	// Cores: /sys/devices/system/cpu/cpu*/topology/core_id per package.
	coreSeen := map[string]bool{}
	entries, _ := filepath.Glob(r.sys() + "/devices/system/cpu/cpu[0-9]*")
	for _, e := range entries {
		pkg := firstLine(e + "/topology/physical_package_id")
		core := firstLine(e + "/topology/core_id")
		if pkg == "" || core == "" {
			continue
		}
		coreSeen[pkg+"/"+core] = true
		_ = cores
	}
	if len(coreSeen) > 0 {
		cpu.Cores = len(coreSeen)
	} else {
		cpu.Cores = cpu.Threads
	}
	return cpu
}

func (c *Collector) memInfo(r Roots) MemoryInfo {
	var m MemoryInfo
	f, err := os.Open(r.proc() + "/meminfo")
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "MemTotal":
			m.Total = v * 1024
		case "MemAvailable":
			m.Available = v * 1024
		case "SwapTotal":
			m.SwapTotal = v * 1024
		}
	}
	return m
}

func (c *Collector) disks(r Roots) []DiskInfo {
	var disks []DiskInfo
	entries, err := os.ReadDir(r.sys() + "/block")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "dm-") {
			continue
		}
		base := r.sys() + "/block/" + name
		size, _ := strconv.ParseUint(strings.TrimSpace(firstLine(base+"/size")), 10, 64)
		rot := strings.TrimSpace(firstLine(base+"/queue/rotational")) == "1"
		disks = append(disks, DiskInfo{
			Path:       "/dev/" + name,
			Model:      strings.TrimSpace(firstLine(base + "/device/model")),
			Serial:     strings.TrimSpace(firstLine(base + "/device/serial")),
			Size:       size * 512, // /sys/block/<d>/size is in 512-byte sectors
			Rotational: rot,
		})
	}
	return disks
}

func (c *Collector) nics(r Roots) []NICInfo {
	var nics []NICInfo
	entries, err := os.ReadDir(r.sys() + "/class/net")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		base := r.sys() + "/class/net/" + name
		speed := 0
		if v, err := strconv.Atoi(strings.TrimSpace(firstLine(base + "/speed"))); err == nil && v > 0 {
			speed = v
		}
		up := strings.TrimSpace(firstLine(base+"/operstate")) == "up"
		nics = append(nics, NICInfo{
			Name:      name,
			MAC:       strings.TrimSpace(firstLine(base + "/address")),
			SpeedMbps: speed,
			Up:        up,
		})
	}
	return nics
}

func (c *Collector) gpus(r Roots) []GPUInfo {
	var gpus []GPUInfo
	// PCI display controllers: /sys/bus/pci/devices/*/class == 0x030000.
	entries, err := os.ReadDir(r.sys() + "/bus/pci/devices")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		base := r.sys() + "/bus/pci/devices/" + e.Name()
		class := strings.TrimSpace(firstLine(base + "/class"))
		if !strings.HasPrefix(class, "0x03") {
			continue
		}
		vendorID := strings.TrimSpace(firstLine(base + "/vendor"))
		gpu := GPUInfo{PCIID: e.Name()}
		switch vendorID {
		case "0x10de":
			gpu.Vendor = "nvidia"
		case "0x8086":
			gpu.Vendor = "intel"
		case "0x1002", "0x1022":
			gpu.Vendor = "amd"
		default:
			gpu.Vendor = vendorID
		}
		// sysfs exposes the PCI device ID (no vendor name database); keep
		// the raw device id as the model identifier.
		gpu.Model = strings.TrimSpace(firstLine(base + "/device"))
		gpus = append(gpus, gpu)
	}
	return gpus
}

func (c *Collector) virtualization(r Roots) string {
	// DMI sysfs first.
	if v := strings.TrimSpace(firstLine(r.sys() + "/class/dmi/id/sys_vendor")); v != "" {
		switch strings.ToLower(v) {
		case "qemu", "bochs":
			return "kvm"
		case "vmware, inc.", "vmware inc.":
			return "vmware"
		case "microsoft corporation":
			return "hyperv"
		case "innotek gmbh", "oracle corporation":
			return "virtualbox"
		case "xen":
			return "xen"
		}
		if strings.Contains(strings.ToLower(v), "amazon") {
			return "kvm"
		}
	}
	if _, err := os.Stat(r.proc() + "/xen"); err == nil {
		return "xen"
	}
	if data, err := os.ReadFile(r.proc() + "/modules"); err == nil {
		if strings.Contains(string(data), "kvm") {
			return "kvm-host"
		}
	}
	return "none"
}

func (c *Collector) tpm(r Roots) *TPMInfo {
	entries, err := os.ReadDir(r.sys() + "/class/tpm")
	if err != nil || len(entries) == 0 {
		return nil
	}
	info := &TPMInfo{Present: true}
	for _, e := range entries {
		v := strings.TrimSpace(firstLine(r.sys() + "/class/tpm/" + e.Name() + "/tpm_version_major"))
		if v != "" {
			info.Version = v
			break
		}
	}
	return info
}

// capabilities derives scheduling-relevant capabilities (drives Phase 04
// constraints). Generous and precise — cheap now, expensive to retrofit.
func capabilities(inv *Inventory) []string {
	var caps []string
	for _, f := range inv.CPU.Flags {
		switch f {
		case "aes":
			caps = append(caps, "aes-ni")
		case "vmx":
			caps = append(caps, "kvm")
		case "svm":
			caps = append(caps, "kvm")
		}
	}
	if inv.Virtualization == "kvm-host" {
		caps = append(caps, "kvm-host")
	}
	if inv.TPM != nil && inv.TPM.Present {
		caps = append(caps, "tpm2")
	}
	for _, g := range inv.GPUs {
		if g.Vendor == "nvidia" {
			caps = append(caps, "nvidia")
		}
	}
	// ZFS: userland tool presence is checked at a higher level; the kernel
	// module shows up in /proc/modules.
	if data, err := os.ReadFile("/proc/modules"); err == nil && strings.Contains(string(data), "zfs") {
		caps = append(caps, "zfs")
	}
	return caps
}

// Loadavg is exported for the health checker (same /proc source).
func Loadavg(procRoot string) (one float64, err error) {
	fields := strings.Fields(firstLine(filepath.Join(procRoot, "loadavg")))
	if len(fields) == 0 {
		return 0, fmt.Errorf("no loadavg")
	}
	return strconv.ParseFloat(fields[0], 64)
}
