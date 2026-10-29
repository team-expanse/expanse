package qmp

import (
	"slices"
	"testing"
)

// fourSlots is query-hotpluggable-cpus for -smp 1,maxcpus=4 with cores 0 and 1 present, as QEMU lists them.
// One line: the fake's replies are newline-separated.
const fourSlots = `{"return":[{"props":{"core-id":3,"thread-id":0,"socket-id":0},"vcpus-count":1,"type":"qemu64-x86_64-cpu"},{"props":{"core-id":2,"thread-id":0,"socket-id":0},"vcpus-count":1,"type":"qemu64-x86_64-cpu"},{"props":{"core-id":1,"thread-id":0,"socket-id":0},"vcpus-count":1,"qom-path":"/machine/peripheral/vcpu1","type":"qemu64-x86_64-cpu"},{"props":{"core-id":0,"thread-id":0,"socket-id":0},"vcpus-count":1,"qom-path":"/machine/unattached/device[0]","type":"qemu64-x86_64-cpu"}]}`

func dialFake(t *testing.T, replies map[string]string) (*Monitor, <-chan []string) {
	t.Helper()
	sock, got := fakeQEMUArgs(t, replies)
	m, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	return m, got
}

func TestCPUsCountsPresentAndPossible(t *testing.T) {
	m, _ := dialFake(t, map[string]string{"query-hotpluggable-cpus": fourSlots})
	defer m.Close()
	present, possible, err := m.CPUs()
	if err != nil || present != 2 || possible != 4 {
		t.Fatalf("CPUs() = %d, %d, %v; want 2, 4", present, possible, err)
	}
}

func TestSetCPUsPlugsTheLowestFreeSlots(t *testing.T) {
	m, got := dialFake(t, map[string]string{"query-hotpluggable-cpus": fourSlots})
	if err := m.SetCPUs(4); err != nil {
		t.Fatal(err)
	}
	m.Close()
	want := []string{
		"qmp_capabilities", "query-hotpluggable-cpus",
		`device_add {"core-id":2,"driver":"qemu64-x86_64-cpu","id":"vcpu2","socket-id":0,"thread-id":0}`,
		`device_add {"core-id":3,"driver":"qemu64-x86_64-cpu","id":"vcpu3","socket-id":0,"thread-id":0}`,
	}
	if cmds := <-got; !slices.Equal(cmds, want) {
		t.Errorf("commands = %q\nwant %q", cmds, want)
	}
}

func TestSetCPUsUnplugsTheHighestPresentOnes(t *testing.T) {
	m, got := dialFake(t, map[string]string{"query-hotpluggable-cpus": fourSlots})
	if err := m.SetCPUs(1); err != nil {
		t.Fatal(err)
	}
	m.Close()
	want := []string{"qmp_capabilities", "query-hotpluggable-cpus", `device_del {"id":"/machine/peripheral/vcpu1"}`}
	if cmds := <-got; !slices.Equal(cmds, want) {
		t.Errorf("commands = %q\nwant %q", cmds, want)
	}
}

func TestSetCPUsRefusesACountOutsideTheSlots(t *testing.T) {
	for _, n := range []int{0, 5} {
		m, _ := dialFake(t, map[string]string{"query-hotpluggable-cpus": fourSlots})
		if err := m.SetCPUs(n); err == nil {
			t.Errorf("SetCPUs(%d) = nil, want an error", n)
		}
		m.Close()
	}
}

func TestMemoryIsRequestedAndReadInMiB(t *testing.T) {
	m, got := dialFake(t, map[string]string{"qom-get": `{"return":268435456}`})
	if err := m.RequestMemory(512); err != nil {
		t.Fatal(err)
	}
	mib, err := m.PluggedMemory()
	if err != nil || mib != 256 {
		t.Fatalf("PluggedMemory() = %d, %v; want 256", mib, err)
	}
	m.Close()
	want := []string{
		"qmp_capabilities",
		`qom-set {"path":"vmem0dev","property":"requested-size","value":536870912}`,
		`qom-get {"path":"vmem0dev","property":"size"}`,
	}
	if cmds := <-got; !slices.Equal(cmds, want) {
		t.Errorf("commands = %q\nwant %q", cmds, want)
	}
}

func TestCPUIDMatchesTheCommandLine(t *testing.T) {
	if got := CPUID(3); got != "vcpu3" {
		t.Errorf("CPUID(3) = %q", got)
	}
}
