package main

import (
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBootShapeDefaultsItsLimitsToTwiceTheRequest(t *testing.T) {
	got := bootShape([]string{"--cpu", "2", "--mem", "1Gi"})
	want := vmShape{CPUs: 2, MaxCPUs: 4, BaseMiB: 1024, MemMiB: 1024, MaxMemMiB: 2048}
	if got != want {
		t.Errorf("bootShape = %+v, want %+v", got, want)
	}
}

func TestBootShapeTakesTheLimitsOnAnEvenMiBGrid(t *testing.T) {
	got := bootShape([]string{"--cpu", "1500m", "--cpu-max", "3", "--mem", "1025Mi", "--mem-max", "3071Mi"})
	want := vmShape{CPUs: 2, MaxCPUs: 3, BaseMiB: 1026, MemMiB: 1026, MaxMemMiB: 3070}
	if got != want {
		t.Errorf("bootShape = %+v, want %+v", got, want)
	}
}

func TestBootShapeNeverLimitsBelowTheRequest(t *testing.T) {
	got := bootShape([]string{"--cpu", "4", "--cpu-max", "2", "--mem", "2Gi", "--mem-max", "1Gi"})
	if got.MaxCPUs != 4 || got.MaxMemMiB != 2048 {
		t.Errorf("bootShape = %+v, want the limits raised to the request", got)
	}
}

func TestShapeQEMUArgsHotplugLayout(t *testing.T) {
	s := vmShape{CPUs: 2, MaxCPUs: 4, BaseMiB: 1024, MemMiB: 1536, MaxMemMiB: 2048}
	want := []string{
		"-smp", "1,maxcpus=4,sockets=1,cores=4,threads=1",
		"-device", `{"core-id":1,"driver":"qemu64-x86_64-cpu","id":"vcpu1","socket-id":0,"thread-id":0}`,
		"-m", "1024M,maxmem=2048M",
		"-object", "memory-backend-ram,id=vmem0,size=1024M",
		"-device", "virtio-mem-pci,id=vmem0dev,memdev=vmem0,requested-size=512M",
	}
	if got := s.qemuArgs("qemu64-x86_64-cpu"); !slices.Equal(got, want) {
		t.Errorf("qemuArgs =\n%q\nwant\n%q", got, want)
	}
}

func TestShapeWithoutMemoryHeadroomHasNoVirtioMem(t *testing.T) {
	s := vmShape{CPUs: 1, MaxCPUs: 1, BaseMiB: 512, MemMiB: 512, MaxMemMiB: 512}
	want := []string{"-smp", "1,maxcpus=1,sockets=1,cores=1,threads=1", "-m", "512M"}
	if got := s.qemuArgs("host-x86_64-cpu"); !slices.Equal(got, want) {
		t.Errorf("qemuArgs = %q, want %q", got, want)
	}
}

func TestCPUTypeFollowsTheModel(t *testing.T) {
	if got := vmCPUType(map[string]any{}); got != "qemu64-x86_64-cpu" {
		t.Errorf("default = %q", got)
	}
	if got := vmCPUType(map[string]any{"cpuModel": "host"}); got != "host-x86_64-cpu" {
		t.Errorf("host = %q", got)
	}
}

func TestShapeFileRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shape")
	s := vmShape{CPUs: 3, MaxCPUs: 4, BaseMiB: 1024, MemMiB: 1536, MaxMemMiB: 2048}
	if err := s.write(path); err != nil {
		t.Fatal(err)
	}
	got, err := readShape(path)
	if err != nil || got != s {
		t.Errorf("readShape = %+v, %v; want %+v", got, err, s)
	}
	if _, err := readShape(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("readShape of a missing file = nil error")
	}
}

// fakeMonitor reaches the requested size after lag polls, or never when stuck.
type fakeMonitor struct {
	cpus, mem, wantCPUs, wantMem, lag int
	stuck                             bool
	calls                             []string
}

func (f *fakeMonitor) step() {
	if f.lag > 0 {
		f.lag--
		return
	}
	if !f.stuck {
		f.cpus, f.mem = f.wantCPUs, f.wantMem
	}
}
func (f *fakeMonitor) CPUs() (int, int, error) { f.step(); return f.cpus, 8, nil }
func (f *fakeMonitor) SetCPUs(n int) error {
	f.calls = append(f.calls, "cpus "+itoa(n))
	f.wantCPUs = n
	return nil
}

func (f *fakeMonitor) RequestMemory(mib int) error {
	f.calls = append(f.calls, "mem "+itoa(mib))
	f.wantMem = mib
	return nil
}
func (f *fakeMonitor) PluggedMemory() (int, error) { return f.mem, nil }
func (f *fakeMonitor) Close() error                { return nil }

func itoa(n int) string { return strconv.Itoa(n) }

func newResizer(f *fakeMonitor, onlined *int, saved *vmShape) *vmResizer {
	f.cpus, f.wantCPUs, f.mem, f.wantMem = 2, 2, 0, 0
	return &vmResizer{
		shape:   vmShape{CPUs: 2, MaxCPUs: 4, BaseMiB: 1024, MemMiB: 1024, MaxMemMiB: 2048},
		dial:    func() (vmMonitor, error) { return f, nil },
		online:  func() { *onlined++ },
		save:    func(s vmShape) { *saved = s },
		timeout: 50 * time.Millisecond,
		poll:    time.Millisecond,
	}
}

func TestResizeGrowsCPUsAndMemoryLive(t *testing.T) {
	f := &fakeMonitor{}
	var onlined int
	var saved vmShape
	r := newResizer(f, &onlined, &saved)
	f.lag = 3
	if err := r.apply(nil, []string{"--cpu", "3", "--mem", "1536Mi"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"cpus 3", "mem 512"}) {
		t.Errorf("calls = %q", f.calls)
	}
	want := vmShape{CPUs: 3, MaxCPUs: 4, BaseMiB: 1024, MemMiB: 1536, MaxMemMiB: 2048}
	if r.shape != want || saved != want {
		t.Errorf("shape = %+v, saved %+v; want %+v", r.shape, saved, want)
	}
	if onlined == 0 {
		t.Error("the guest was never asked to online what was added")
	}
}

func TestResizeLeavesAnUnchangedPartAlone(t *testing.T) {
	f := &fakeMonitor{}
	var onlined int
	var saved vmShape
	r := newResizer(f, &onlined, &saved)
	if err := r.apply(nil, []string{"--cpu", "1", "--mem", "1Gi"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"cpus 1"}) {
		t.Errorf("calls = %q", f.calls)
	}
}

func TestResizeRefusesWhatTheBootLayoutCannotHold(t *testing.T) {
	for _, args := range [][]string{
		{"--cpu", "5", "--mem", "1Gi"},   // past the vCPU slots
		{"--cpu", "2", "--mem", "3Gi"},   // past maxmem
		{"--cpu", "2", "--mem", "512Mi"}, // below the base it booted with
	} {
		f := &fakeMonitor{}
		var onlined int
		var saved vmShape
		r := newResizer(f, &onlined, &saved)
		if err := r.apply(nil, args); err == nil || len(f.calls) != 0 {
			t.Errorf("apply(%q) = %v with calls %q, want a refusal before any change", args, err, f.calls)
		}
	}
}

func TestResizeTheGuestNeverFinishesFails(t *testing.T) {
	f := &fakeMonitor{stuck: true}
	var onlined int
	var saved vmShape
	r := newResizer(f, &onlined, &saved)
	err := r.apply(nil, []string{"--cpu", "1", "--mem", "1Gi"})
	if err == nil || !strings.Contains(err.Error(), "1 vCPUs") {
		t.Errorf("apply = %v, want a timeout naming the size", err)
	}
	if saved != (vmShape{}) {
		t.Errorf("a resize that never finished was saved: %+v", saved)
	}
}

func TestResizeReportsADialFailure(t *testing.T) {
	r := &vmResizer{
		shape: vmShape{CPUs: 1, MaxCPUs: 2, BaseMiB: 512, MemMiB: 512, MaxMemMiB: 1024},
		dial:  func() (vmMonitor, error) { return nil, errors.New("no socket") },
	}
	if err := r.apply(nil, []string{"--cpu", "2", "--mem", "512Mi"}); err == nil {
		t.Error("apply = nil, want the dial error")
	}
}
