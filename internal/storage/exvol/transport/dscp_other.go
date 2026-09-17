//go:build !linux

package transport

import "net"

// DSCP classes (§4.3 bandwidth control).
const (
	DSCPForeground = 0
	DSCPBulk       = 32
)

// markDSCP is a no-op on non-Linux platforms.
func markDSCP(c *net.TCPConn, dscp int) {}
