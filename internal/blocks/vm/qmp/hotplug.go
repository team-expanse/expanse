package qmp

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
)

// MemDevice is the virtio-mem device holding a VM's memory beyond its base.
const MemDevice = "vmem0dev"

// CPUID names the vCPU in slot i, on the command line and when hot-added alike.
func CPUID(i int) string { return "vcpu" + strconv.Itoa(i) }

type hotCPU struct {
	Type    string         `json:"type"`
	Props   map[string]int `json:"props"`
	QOMPath string         `json:"qom-path"`
}

// hotCPUs lists the VM's vCPU slots in -smp order: socket, then core, then thread.
func (m *Monitor) hotCPUs() ([]hotCPU, error) {
	var cpus []hotCPU
	if err := m.Execute("query-hotpluggable-cpus", nil, &cpus); err != nil {
		return nil, err
	}
	slices.SortFunc(cpus, func(a, b hotCPU) int {
		for _, k := range []string{"socket-id", "core-id", "thread-id"} {
			if c := cmp.Compare(a.Props[k], b.Props[k]); c != 0 {
				return c
			}
		}
		return 0
	})
	return cpus, nil
}

// CPUs reports how many vCPUs are present and how many slots the VM has.
func (m *Monitor) CPUs() (present, slots int, err error) {
	cpus, err := m.hotCPUs()
	for _, c := range cpus {
		if c.QOMPath != "" {
			present++
		}
	}
	return present, len(cpus), err
}

// SetCPUs plugs the lowest free slots, or unplugs the highest present vCPUs, until n are present
// or on their way; the guest finishes an unplug in its own time.
func (m *Monitor) SetCPUs(n int) error {
	cpus, err := m.hotCPUs()
	if err != nil {
		return err
	}
	if n < 1 || n > len(cpus) {
		return fmt.Errorf("qmp: %d vCPUs: the VM has slots for 1 to %d", n, len(cpus))
	}
	for i, c := range cpus {
		switch {
		case i < n && c.QOMPath == "":
			args := map[string]any{"driver": c.Type, "id": CPUID(i)}
			for k, v := range maps.All(c.Props) {
				args[k] = v
			}
			err = m.Execute("device_add", args, nil)
		case i >= n && c.QOMPath != "":
			err = m.Execute("device_del", map[string]any{"id": c.QOMPath}, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// RequestMemory asks the guest to hold mib MiB in the virtio-mem device.
func (m *Monitor) RequestMemory(mib int) error {
	return m.Execute("qom-set", map[string]any{"path": MemDevice, "property": "requested-size", "value": int64(mib) << 20}, nil)
}

// PluggedMemory is how many MiB the guest holds in the virtio-mem device.
func (m *Monitor) PluggedMemory() (int, error) {
	var bytes int64
	err := m.Execute("qom-get", map[string]any{"path": MemDevice, "property": "size"}, &bytes)
	return int(bytes >> 20), err
}
