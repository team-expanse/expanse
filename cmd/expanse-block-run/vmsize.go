package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/expanse/expanse/internal/blocks/vm/qga"
	"github.com/expanse/expanse/internal/blocks/vm/qmp"
	"github.com/expanse/expanse/internal/quantity"
)

// vmShape is a VM's size and the hotplug room it booted with: vCPU slots, the base memory it
// cannot shrink below, and a virtio-mem region up to MaxMemMiB.
type vmShape struct {
	CPUs      int `json:"cpus"`
	MaxCPUs   int `json:"maxCPUs"`
	BaseMiB   int `json:"baseMiB"`
	MemMiB    int `json:"memMiB"`
	MaxMemMiB int `json:"maxMemMiB"`
}

// bootShape sizes a cold boot from the spec: requests now, limits (default twice the request)
// as the ceiling. Memory sits on virtio-mem's 2 MiB grid.
func bootShape(args []string) vmShape {
	cpus, mem := vmCPUs(args), evenUp(vmMemMiB(args))
	maxCPUs := 2 * cpus
	if raw := vmArg(args, "--cpu-max"); raw != "" {
		maxCPUs = vmCPUs([]string{"--cpu", raw})
	}
	maxMem := 2 * mem
	if raw := vmArg(args, "--mem-max"); raw != "" {
		if q, err := quantity.ParseBytes(raw); err == nil {
			maxMem = int(q.N>>20) &^ 1
		}
	}
	return vmShape{CPUs: cpus, MaxCPUs: max(maxCPUs, cpus), BaseMiB: mem, MemMiB: mem, MaxMemMiB: max(maxMem, mem)}
}

func evenUp(mib int) int { return (mib + 1) &^ 1 }

// qemuArgs boot the shape: one vCPU plus named hot-pluggable ones on cores of one socket, and
// the memory beyond the base in a virtio-mem device.
func (s vmShape) qemuArgs(cpuType string) []string {
	out := []string{"-smp", fmt.Sprintf("1,maxcpus=%d,sockets=1,cores=%d,threads=1", s.MaxCPUs, s.MaxCPUs)}
	for i := 1; i < s.CPUs; i++ {
		dev, _ := json.Marshal(map[string]any{"driver": cpuType, "id": qmp.CPUID(i), "socket-id": 0, "core-id": i, "thread-id": 0})
		out = append(out, "-device", string(dev))
	}
	region := s.MaxMemMiB - s.BaseMiB
	if region == 0 {
		return append(out, "-m", fmt.Sprintf("%dM", s.BaseMiB))
	}
	return append(out,
		"-m", fmt.Sprintf("%dM,maxmem=%dM", s.BaseMiB, s.MaxMemMiB),
		"-object", fmt.Sprintf("memory-backend-ram,id=vmem0,size=%dM", region),
		"-device", fmt.Sprintf("virtio-mem-pci,id=%s,memdev=vmem0,requested-size=%dM", qmp.MemDevice, s.MemMiB-s.BaseMiB))
}

// vmCPUType is qemu's vCPU device type for the guest's CPU model, as hotplug names it.
func vmCPUType(cfg map[string]any) string {
	if cfgStr(cfg, "cpuModel") == "host" {
		return "host-x86_64-cpu"
	}
	return "qemu64-x86_64-cpu"
}

// shapePath holds a running VM's current shape, which a suspend saves beside its state.
func shapePath(instance string) string { return "/run/expblk-" + instance + "-shape.json" }

func (s vmShape) write(path string) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readShape(path string) (vmShape, error) {
	var s vmShape
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if s.CPUs < 1 || s.MaxCPUs < s.CPUs || s.BaseMiB < 1 || s.MemMiB < s.BaseMiB || s.MaxMemMiB < s.MemMiB {
		return s, fmt.Errorf("vm shape %s: %+v is not a layout qemu could have run", path, s)
	}
	return s, nil
}

// newVMResizer resizes instance's qemu over QMP and onlines through its guest agent.
func newVMResizer(instance string, shape vmShape, qmpSock, qgaSock string) *vmResizer {
	return &vmResizer{
		shape: shape,
		dial:  func() (vmMonitor, error) { return qmp.Dial(qmpSock) },
		online: func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = qga.Online(ctx, qgaSock)
		},
		save: func(s vmShape) {
			if err := s.write(shapePath(instance)); err != nil {
				fmt.Fprintf(os.Stderr, "expanse-block-run: vm %s: record its layout: %v\n", instance, err)
			}
		},
		timeout: resizeTimeout,
		poll:    500 * time.Millisecond,
	}
}

// resizeTimeout is how long a guest gets to take or give back vCPUs and memory before a restart.
const resizeTimeout = time.Minute

// vmMonitor is the slice of QMP a live resize uses.
type vmMonitor interface {
	CPUs() (present, slots int, err error)
	SetCPUs(n int) error
	RequestMemory(mib int) error
	PluggedMemory() (int, error)
	Close() error
}

// vmResizer moves a running VM to a new size within the layout it booted with.
type vmResizer struct {
	shape         vmShape
	dial          func() (vmMonitor, error)
	online        func() // asks the guest to online what was added; best effort
	save          func(vmShape)
	timeout, poll time.Duration
}

// onlineEvery spaces the guest-agent calls, which wait seconds on a guest without an agent.
const onlineEvery = 2 * time.Second

// apply is the reload hook: it resizes to next's requests, or errs so the unit restarts on them.
func (r *vmResizer) apply(_, next []string) error {
	want := r.shape
	want.CPUs, want.MemMiB = vmCPUs(next), evenUp(vmMemMiB(next))
	if want == r.shape {
		return nil
	}
	if want.CPUs > want.MaxCPUs || want.MemMiB > want.MaxMemMiB || want.MemMiB < want.BaseMiB {
		return fmt.Errorf("%d vCPUs and %d MiB lie outside what it booted with (up to %d vCPUs, %d to %d MiB)",
			want.CPUs, want.MemMiB, want.MaxCPUs, want.BaseMiB, want.MaxMemMiB)
	}
	m, err := r.dial()
	if err != nil {
		return err
	}
	defer m.Close()
	if want.CPUs != r.shape.CPUs {
		if err := m.SetCPUs(want.CPUs); err != nil {
			return err
		}
	}
	if want.MemMiB != r.shape.MemMiB {
		if err := m.RequestMemory(want.MemMiB - want.BaseMiB); err != nil {
			return err
		}
	}
	if err := r.wait(m, want); err != nil {
		return err
	}
	r.shape = want
	r.save(want)
	return nil
}

// wait polls until the guest holds want, onlining as it goes.
func (r *vmResizer) wait(m vmMonitor, want vmShape) error {
	deadline := time.Now().Add(r.timeout)
	var lastOnline time.Time
	for {
		cpus, _, err := m.CPUs()
		if err != nil {
			return err
		}
		mem := 0
		if want.MaxMemMiB > want.BaseMiB {
			if mem, err = m.PluggedMemory(); err != nil {
				return err
			}
		}
		if cpus == want.CPUs && mem == want.MemMiB-want.BaseMiB {
			if r.online != nil {
				r.online()
			}
			return nil
		}
		if r.online != nil && time.Since(lastOnline) >= onlineEvery {
			r.online()
			lastOnline = time.Now()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the guest holds %d vCPUs and %d MiB after %v, not the %d vCPUs and %d MiB asked",
				cpus, want.BaseMiB+mem, r.timeout, want.CPUs, want.MemMiB)
		}
		time.Sleep(r.poll)
	}
}
