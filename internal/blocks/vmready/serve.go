package vmready

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
)

// Guest CIDs 0-2 are reserved (hypervisor, local, host) and 0xFFFFFFFF means any; stay well clear.
const (
	MinCID uint32 = 3
	MaxCID uint32 = 1<<31 - 1
)

// maxMessage bounds one notify message; systemd's are a few dozen bytes.
const maxMessage = 4096

// Listener yields one connection per guest notify message, with the sending guest's CID.
type Listener interface {
	Accept(ctx context.Context) (io.ReadCloser, uint32, error)
}

// Serve tracks the guest at cid and publishes its status on every change, starting with "booting".
// It returns the listener's error, or the context's once cancelled.
func Serve(ctx context.Context, l Listener, cid uint32, publish func(string)) error {
	tr := NewTracker()
	last := tr.Status()
	publish(last)
	for {
		conn, peer, err := l.Accept(ctx)
		if err != nil {
			return err
		}
		msg, _ := io.ReadAll(io.LimitReader(conn, maxMessage))
		conn.Close()
		if peer != cid {
			continue // another guest on this host, or a stale one
		}
		tr.Observe(msg)
		if s := tr.Status(); s != last {
			last = s
			publish(s)
		}
	}
}

// CIDFor is an instance's first-choice guest CID: stable across restarts, spread across the range.
func CIDFor(instance string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(instance))
	return MinCID + h.Sum32()%(MaxCID-MinCID+1)
}

// NextCID is the CID to try after c is taken, wrapping within the usable range.
func NextCID(c uint32) uint32 {
	if c >= MaxCID {
		return MinCID
	}
	return c + 1
}

// QEMUArgs attaches the claimed vhost-vsock fd and tells the guest's systemd where to report:
// the host (CID 2) on a port equal to the guest's CID, which is unique on this host.
func QEMUArgs(cid uint32, vhostFD int) []string {
	return []string{
		"-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d,vhostfd=%d", cid, vhostFD),
		"-smbios", fmt.Sprintf("type=11,value=io.systemd.credential:vmm.notify_socket=vsock-stream:2:%d", cid),
	}
}
