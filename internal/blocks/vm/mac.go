package vm

import (
	"crypto/sha256"
	"net"
)

// DefaultMAC derives a guest's MAC from its replica instance ("<ns>-<name>-<index>"):
// locally administered, unicast, and the same on every node, so failover keeps the address.
func DefaultMAC(instance string) string {
	sum := sha256.Sum256([]byte("expanse-vm-mac:" + instance))
	b := make([]byte, 6)
	copy(b, sum[:6])
	b[0] = (b[0] &^ 0x01) | 0x02
	return net.HardwareAddr(b).String()
}

// MAC is the guest's address: config's mac, else DefaultMAC.
func MAC(cfg map[string]any, instance string) string {
	if m, _ := cfg["mac"].(string); m != "" {
		return m
	}
	return DefaultMAC(instance)
}
