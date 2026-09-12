package inventory

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture builds a fake /proc + /sys tree.
type fixture struct {
	root Roots
}

// writeFile writes content at path, creating parent dirs.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	return fixture{root: Roots{Proc: filepath.Join(root, "proc"), Sys: filepath.Join(root, "sys")}}
}

func TestCollectCPUAndMemory(t *testing.T) {
	f := newFixture(t)
	cpuinfo := `processor	: 0
vendor_id	: GenuineIntel
model name	: Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz
cpu MHz		: 2400.000
core id		: 0
physical id	: 0
flags		: fpu aes avx2 vmx svm

processor	: 1
model name	: Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz
core id		: 1
physical id	: 0
flags		: fpu aes avx2 vmx svm
`
	writeFile(t, filepath.Join(f.root.Proc, "cpuinfo"), cpuinfo)
	writeFile(t, filepath.Join(f.root.Proc, "meminfo"),
		"MemTotal:       16384000 kB\nMemAvailable:    8000000 kB\nSwapTotal:      4096000 kB\n")
	writeFile(t, filepath.Join(f.root.Proc, "sys/kernel/hostname"), "node-a\n")
	writeFile(t, filepath.Join(f.root.Proc, "sys/kernel/osrelease"), "6.6.42\n")
	// Two threads, one core each in the same package → 2 cores, 2 threads.
	writeFile(t, f.root.Sys+"/devices/system/cpu/cpu0/topology/physical_package_id", "0\n")
	writeFile(t, f.root.Sys+"/devices/system/cpu/cpu0/topology/core_id", "0\n")
	writeFile(t, f.root.Sys+"/devices/system/cpu/cpu1/topology/physical_package_id", "0\n")
	writeFile(t, f.root.Sys+"/devices/system/cpu/cpu1/topology/core_id", "1\n")

	c := Collector{Roots: f.root}
	inv, err := c.Collect("node-a")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Hostname != "node-a" {
		t.Errorf("hostname = %q", inv.Hostname)
	}
	if inv.CPU.Model != "Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz" {
		t.Errorf("cpu model = %q", inv.CPU.Model)
	}
	if inv.CPU.Threads != 2 || inv.CPU.Cores != 2 {
		t.Errorf("cores/threads = %d/%d, want 2/2", inv.CPU.Cores, inv.CPU.Threads)
	}
	if inv.Memory.Total != 16384000*1024 {
		t.Errorf("mem total = %d", inv.Memory.Total)
	}
	if inv.OS.Kernel != "6.6.42" {
		t.Errorf("kernel = %q", inv.OS.Kernel)
	}
	// Capabilities: aes + vmx present.
	has := func(c string) bool {
		for _, x := range inv.Capabilities {
			if x == c {
				return true
			}
		}
		return false
	}
	if !has("aes-ni") || !has("kvm") {
		t.Errorf("capabilities = %v, want aes-ni and kvm", inv.Capabilities)
	}
}

func TestCollectDisksAndNICs(t *testing.T) {
	f := newFixture(t)
	// /sys/block/sda + sdb (skip loop0/ram0/dm-0).
	for _, d := range []string{"sda", "sdb", "loop0", "ram0", "dm-0"} {
		base := filepath.Join(f.root.Sys, "block", d)
		writeFile(t, base+"/size", "1953525168\n")
		writeFile(t, base+"/queue/rotational", "0\n")
	}
	writeFile(t, filepath.Join(f.root.Sys, "block", "sda", "device", "model"), "Samsung SSD 970\n")
	writeFile(t, filepath.Join(f.root.Sys, "block", "sda", "device", "serial"), "S12345\n")

	// /sys/class/net: eth0 up, wlan0 down, lo skipped.
	for _, n := range []string{"eth0", "wlan0", "lo"} {
		base := filepath.Join(f.root.Sys, "class", "net", n)
		writeFile(t, base+"/address", "aa:bb:cc:dd:ee:0"+n[len(n)-1:]+"\n")
	}
	writeFile(t, filepath.Join(f.root.Sys, "class", "net", "eth0", "speed"), "1000\n")
	writeFile(t, filepath.Join(f.root.Sys, "class", "net", "eth0", "operstate"), "up\n")
	writeFile(t, filepath.Join(f.root.Sys, "class", "net", "wlan0", "speed"), "0\n")
	writeFile(t, filepath.Join(f.root.Sys, "class", "net", "wlan0", "operstate"), "down\n")

	c := Collector{Roots: f.root}
	inv, err := c.Collect("n")
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Disks) != 2 {
		t.Fatalf("disks = %+v, want sda+sdb only", inv.Disks)
	}
	if inv.Disks[0].Model != "Samsung SSD 970" || inv.Disks[0].Serial != "S12345" {
		t.Errorf("disk0 = %+v", inv.Disks[0])
	}
	if inv.Disks[0].Size != 1953525168*512 {
		t.Errorf("disk0 size = %d", inv.Disks[0].Size)
	}
	if inv.Disks[0].Rotational {
		t.Error("rotational should be false for SSD")
	}
	if len(inv.Network) != 2 {
		t.Fatalf("nics = %+v, want eth0+wlan0", inv.Network)
	}
	if !inv.Network[0].Up || inv.Network[0].SpeedMbps != 1000 {
		t.Errorf("eth0 = %+v", inv.Network[0])
	}
}

func TestCollectGPUsAndTPM(t *testing.T) {
	f := newFixture(t)
	pci := filepath.Join(f.root.Sys, "bus", "pci", "devices")
	writeFile(t, filepath.Join(pci, "0000:01:00.0", "class"), "0x030000\n")
	writeFile(t, filepath.Join(pci, "0000:01:00.0", "vendor"), "0x10de\n")
	writeFile(t, filepath.Join(pci, "0000:01:00.0", "device"), "0x2504\n")
	writeFile(t, filepath.Join(pci, "0000:02:00.0", "class"), "0x010600\n") // SATA, not a GPU

	writeFile(t, filepath.Join(f.root.Sys, "class", "tpm", "tpm0", "tpm_version_major"), "2.0\n")

	c := Collector{Roots: f.root}
	inv, err := c.Collect("n")
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.GPUs) != 1 {
		t.Fatalf("gpus = %+v, want one display controller", inv.GPUs)
	}
	if inv.GPUs[0].Vendor != "nvidia" {
		t.Errorf("gpu vendor = %q", inv.GPUs[0].Vendor)
	}
	if inv.TPM == nil || !inv.TPM.Present || inv.TPM.Version != "2.0" {
		t.Errorf("tpm = %+v", inv.TPM)
	}
}

func TestVirtualizationDetection(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Join(f.root.Sys, "class", "dmi", "id", "sys_vendor"), "QEMU\n")
	c := Collector{Roots: f.root}
	inv, err := c.Collect("n")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Virtualization != "kvm" {
		t.Errorf("virtualization = %q, want kvm", inv.Virtualization)
	}
}

func TestLoadavg(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Join(f.root.Proc, "loadavg"), "0.52 0.58 0.59 1/384 10240\n")
	one, err := Loadavg(f.root.Proc)
	if err != nil {
		t.Fatal(err)
	}
	if one != 0.52 {
		t.Errorf("loadavg1 = %v", one)
	}
}

func TestHostname(t *testing.T) {
	f := newFixture(t)
	// /proc/hostname does not exist; the kernel exposes it under
	// /proc/sys/kernel/hostname.
	writeFile(t, filepath.Join(f.root.Proc, "sys/kernel/hostname"), "node-a\n")
	c := Collector{Roots: f.root}
	inv, err := c.Collect("node-a")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Hostname != "node-a" {
		t.Errorf("hostname = %q, want node-a", inv.Hostname)
	}
}
