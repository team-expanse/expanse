package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/expanse/expanse/internal/blocks/vm"
	"github.com/expanse/expanse/internal/blocks/vmready"
)

// guestWatch follows a VM guest's systemd over vsock: a claimed CID, and a listener on port = CID.
type guestWatch struct {
	vhost *os.File
	cid   uint32
	l     *vmready.VsockListener
}

func startGuestWatch(instance string) (*guestWatch, error) {
	vhost, cid, err := vmready.ClaimCID(instance)
	if err != nil {
		return nil, err
	}
	l, err := vmready.ListenVsock(cid)
	if err != nil {
		_ = vhost.Close()
		return nil, err
	}
	return &guestWatch{vhost: vhost, cid: cid, l: l}, nil
}

// serve publishes the guest's status until ctx ends.
func (w *guestWatch) serve(ctx context.Context, instance string) {
	err := vmready.Serve(ctx, w.l, w.cid, func(s string) { publishGuestStatus(instance, s) })
	if ctx.Err() == nil {
		publishGuestStatus(instance, fmt.Sprintf("not ready: guest watch stopped: %v", err))
	}
}

func (w *guestWatch) close() {
	_ = w.vhost.Close()
	_ = w.l.Close()
}

// publishGuestStatus sets the unit's status text, which the agent reads back as the replica's readiness.
func publishGuestStatus(instance, s string) {
	if _, err := daemon.SdNotify(false, "STATUS="+s); err != nil {
		fmt.Fprintf(os.Stderr, "expanse-block-run: vm %s: sd_notify: %v\n", instance, err)
	}
	fmt.Printf("expanse-block-run: vm %s guest %s\n", instance, s)
}

// imageCache holds downloaded VM images by digest, shared by the node's VMs.
var imageCache = "/var/lib/expanse/images"

// prepareGuest writes the configured image onto a blank disk and builds the cloud-init
// seed ISO, returning its path ("" with neither image nor cloudInit set).
func prepareGuest(ctx context.Context, instance, name string, cfg map[string]any, dev string) (string, func(), error) {
	none := func() {}
	im, err := vm.ImageFromConfig(cfg)
	if err != nil {
		return "", none, err
	}
	ci, err := vm.CloudInitFromConfig(cfg)
	if err != nil {
		return "", none, err
	}
	if im != nil {
		if err := installImage(ctx, instance, *im, dev); err != nil {
			publishGuestStatus(instance, "not ready: "+err.Error())
			return "", none, err
		}
	}
	if _, set := cfg["cloudInit"]; im == nil && !set {
		return "", none, nil
	}
	xorriso, err := resolveBin("xorriso")
	if err != nil {
		return "", none, err
	}
	dir, err := os.MkdirTemp("", "seed-")
	if err != nil {
		return "", none, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	iso, err := vm.BuildSeed(ctx, xorriso, dir, instance, ci, name)
	if err != nil {
		cleanup()
		return "", none, err
	}
	return iso, cleanup, nil
}

// installImage writes im onto dev unless dev already holds something (a later start, or a failover).
func installImage(ctx context.Context, instance string, im vm.Image, dev string) error {
	blank, err := vm.Blank(dev)
	if err != nil || !blank {
		return err
	}
	if err := os.MkdirAll(imageCache, 0o700); err != nil {
		return err
	}
	publishGuestStatus(instance, "not ready: downloading the image")
	src, err := vm.Fetch(ctx, nil, im, imageCache, downloadProgress(instance, time.Now))
	if err != nil {
		return err
	}
	qemuImg, err := resolveBin("qemu-img")
	if err != nil {
		return err
	}
	publishGuestStatus(instance, "not ready: writing the image to the disk")
	if err := vm.Install(ctx, qemuImg, src, dev, imageCache); err != nil {
		return fmt.Errorf("write the image: %w", err)
	}
	fmt.Printf("expanse-block-run: vm %s: image %s written to %s\n", instance, im.URL, dev)
	return nil
}

// downloadProgress publishes the download's percentage at most every five seconds.
func downloadProgress(instance string, now func() time.Time) func(done, total int64) {
	var last time.Time
	return func(done, total int64) {
		if t := now(); t.Sub(last) >= 5*time.Second || done == total {
			last = t
			publishGuestStatus(instance, downloadStatus(done, total))
		}
	}
}

func downloadStatus(done, total int64) string {
	if total <= 0 {
		return fmt.Sprintf("not ready: downloading the image (%d MiB)", done>>20)
	}
	return fmt.Sprintf("not ready: downloading the image (%d%% of %d MiB)", done*100/total, total>>20)
}
