//go:build linux

package transport

import (
	"net"
	"syscall"
)

// DSCP classes (§4.3 bandwidth control): resync traffic is marked so it
// does not starve foreground I/O. CS1 (class selector 1) is the
// conventional "bulk/background" class.
const (
	DSCPForeground = 0  // EF-like: no marking / default
	DSCPBulk       = 32 // CS1 (value << 2 happens in markDSCP)
)

// markDSCP sets the DSCP field on a TCP connection's outbound packets.
// Best-effort: any failure is ignored (traffic flows unmarked) — marking
// is an optimization, not a correctness requirement.
func markDSCP(c *net.TCPConn, dscp int) {
	raw, err := c.SyscallConn()
	if err != nil {
		return
	}
	tos := dscp << 2 // DSCP occupies the top 6 bits of the 8-bit TOS field
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TOS, tos)
	})
}
