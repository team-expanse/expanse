package main

import (
	"context"
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
