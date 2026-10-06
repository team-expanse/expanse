package main

import (
	"context"
	"fmt"
	"os"

	"github.com/coreos/go-systemd/v22/daemon"

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
