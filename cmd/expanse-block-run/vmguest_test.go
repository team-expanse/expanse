package main

import (
	"context"
	"slices"
	"testing"
)

func TestDownloadStatus(t *testing.T) {
	if got := downloadStatus(512<<20, 1024<<20); got != "not ready: downloading the image (50% of 1024 MiB)" {
		t.Error(got)
	}
	if got := downloadStatus(3<<20, -1); got != "not ready: downloading the image (3 MiB)" {
		t.Error(got)
	}
}

func TestPrepareGuestWithoutImageOrCloudInitAddsNoSeed(t *testing.T) {
	seed, cleanup, err := prepareGuest(context.Background(), "default-vm1-0", "vm1", map[string]any{"cpuModel": "host"}, "/nonexistent")
	defer cleanup()
	if seed != "" || err != nil {
		t.Fatalf("seed %q, %v", seed, err)
	}
}

func TestPrepareGuestRefusesABadImageSetting(t *testing.T) {
	_, cleanup, err := prepareGuest(context.Background(), "default-vm1-0", "vm1",
		map[string]any{"image": map[string]any{"url": "https://x/a.qcow2"}}, "/nonexistent")
	defer cleanup()
	if err == nil {
		t.Fatal("an image without a checksum was accepted")
	}
}

func TestGuestStateCarriesTheAddressesAcrossStateChanges(t *testing.T) {
	var sent []string
	g := &guestState{notify: func(s string) { sent = append(sent, s) }}
	g.setState("booting")
	if !g.setAddrs([]string{"192.168.1.50"}) || g.setAddrs([]string{"192.168.1.50"}) {
		t.Error("setAddrs should report only a change")
	}
	g.setState("ready: multi-user.target reached")
	g.setAddrs(nil)
	want := []string{
		"booting",
		"booting (addresses: 192.168.1.50)",
		"ready: multi-user.target reached (addresses: 192.168.1.50)",
		"ready: multi-user.target reached",
	}
	if !slices.Equal(sent, want) {
		t.Errorf("sent %q, want %q", sent, want)
	}
}
