package vmready

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unsafe"

	"github.com/mdlayher/socket"
	"golang.org/x/sys/unix"
)

// vhostVsockSetGuestCID is VHOST_VSOCK_SET_GUEST_CID, _IOW(VHOST_VIRTIO, 0x60, __u64).
const vhostVsockSetGuestCID = 0x4008AF60

// claimAttempts bounds the search for a free CID; collisions are rare, a full host is not normal.
const claimAttempts = 64

// readTimeout bounds one message; a guest holding a connection open must not stall the others.
const readTimeout = 5 * time.Second

// ClaimCID opens /dev/vhost-vsock and binds the first free CID from CIDFor(instance) on,
// so QEMU can be handed the fd (vhostfd=) instead of failing on a CID already in use.
func ClaimCID(instance string) (*os.File, uint32, error) {
	f, err := os.OpenFile("/dev/vhost-vsock", os.O_RDWR, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open /dev/vhost-vsock: %w", err)
	}
	cid := CIDFor(instance)
	for range claimAttempts {
		c64 := uint64(cid)
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), vhostVsockSetGuestCID, uintptr(unsafe.Pointer(&c64)))
		if errno == 0 {
			return f, cid, nil
		}
		if errno != unix.EADDRINUSE {
			f.Close()
			return nil, 0, fmt.Errorf("set guest CID %d: %w", cid, errno)
		}
		cid = NextCID(cid)
	}
	f.Close()
	return nil, 0, fmt.Errorf("no free guest CID after %d attempts from %d", claimAttempts, CIDFor(instance))
}

// VsockListener accepts guest connections on one host vsock port.
type VsockListener struct{ c *socket.Conn }

// ListenVsock listens on port for connections from any guest.
func ListenVsock(port uint32) (*VsockListener, error) {
	c, err := socket.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0, "vsock", nil)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := c.Bind(&unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		c.Close()
		return nil, fmt.Errorf("vsock bind port %d: %w", port, err)
	}
	if err := c.Listen(16); err != nil {
		c.Close()
		return nil, fmt.Errorf("vsock listen port %d: %w", port, err)
	}
	return &VsockListener{c: c}, nil
}

// Accept implements Listener.
func (l *VsockListener) Accept(ctx context.Context) (io.ReadCloser, uint32, error) {
	conn, sa, err := l.c.Accept(ctx, 0)
	if err != nil {
		return nil, 0, err
	}
	vm, ok := sa.(*unix.SockaddrVM)
	if !ok {
		conn.Close()
		return nil, 0, errors.New("vsock accept: peer is not a vsock address")
	}
	if err := conn.SetDeadline(time.Now().Add(readTimeout)); err != nil {
		conn.Close()
		return nil, 0, err
	}
	return conn, vm.CID, nil
}

// Close stops listening.
func (l *VsockListener) Close() error { return l.c.Close() }
