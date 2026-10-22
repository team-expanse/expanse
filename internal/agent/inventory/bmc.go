package inventory

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// BMCInfo is the node's baseboard management controller (iDRAC, iLO, ...) on its LAN channel.
type BMCInfo struct {
	IP     string `json:"ip"`
	MAC    string `json:"mac"`
	Source string `json:"source"`
}

// bmc reads the BMC through the local IPMI device; nil when the host has none or it doesn't answer.
func (c *Collector) bmc(r Roots) *BMCInfo {
	if _, err := os.Stat(r.dev() + "/ipmi0"); err != nil {
		return nil
	}
	lanPrint := c.LanPrint
	if lanPrint == nil {
		lanPrint = ipmitoolLanPrint
	}
	// ipmitool exits 1 when any one parameter is unsupported, after printing the rest.
	out, _ := lanPrint()
	return parseLanPrint(out)
}

func ipmitoolLanPrint() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ipmitool", "lan", "print").Output()
	return string(out), err
}

// parseLanPrint reads `ipmitool lan print`'s "Key : value" lines; IPMI has no sysfs or JSON source.
func parseLanPrint(out string) *BMCInfo {
	var b BMCInfo
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "IP Address":
			if v != "0.0.0.0" {
				b.IP = v
			}
		case "MAC Address":
			if v != "00:00:00:00:00:00" {
				b.MAC = v
			}
		case "IP Address Source":
			b.Source = v
		}
	}
	if b.IP == "" && b.MAC == "" {
		return nil
	}
	return &b
}
