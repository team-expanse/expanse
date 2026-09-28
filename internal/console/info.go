// Package console is the node's tty1 host information screen (like ESXi's DCUI): Collect
// gathers what the node knows about itself, Render draws it. It is read-only and shows
// no secrets; the render is pure so the layout is unit-tested at the VT's 80x25.
package console

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Info is everything the screen shows.
type Info struct {
	Version  string
	Hostname string
	NodeID   string
	Uptime   time.Duration
	Now      time.Time

	Addrs   []Addr
	Agent   AgentState
	Health  Health
	Cluster *Cluster // nil: not in a cluster yet

	CPU         CPU
	Memory      Memory
	Disks       []Disk
	Arrays      []MDArray
	Filesystems []Filesystem
}

// Addr is one address on one interface.
type Addr struct{ Iface, IP string }

// AgentState says whether expansed is reachable; Err with Up set means it answered
// slowly or partially (never shown as "not running").
type AgentState struct {
	Up  bool
	Err string
}

// Health is the agent's heartbeat verdict plus the (cached) names of checks that did not pass.
type Health struct {
	Overall  string // healthy | degraded | unhealthy | "" (no heartbeat yet)
	NoQuorum bool   // the heartbeat says degraded=true: the node cannot reach quorum
	Failing  []string
}

// Cluster is the node's view of its cluster.
type Cluster struct {
	Name       string
	Role       string // leader | voter | witness | nonvoter, with lifecycle annotations
	QuorumHave int
	QuorumNeed int
	Nodes      int
	Degraded   bool // no leader known: reads only
}

type CPU struct {
	Model   string
	Threads int
}

type Memory struct{ Total, Available int64 }

type Disk struct {
	Name  string
	Size  int64
	Model string
}

// MDArray is one md RAID array from /proc/mdstat.
type MDArray struct {
	Name    string
	Label   string // the /dev/md/<label> name, e.g. "system"; "" when there is none
	Level   string
	Members string // e.g. "[UU]" or "[U_]"
	Total   int
	Active  int
	Sync    string // e.g. "recovery 12.5%", "" when idle
}

// Degraded reports whether members are missing.
func (a MDArray) Degraded() bool { return a.Active < a.Total }

type Filesystem struct {
	Mount       string
	Total, Free int64
}

// parseMdstat reads /proc/mdstat: each array's level, member map and any resync progress.
func parseMdstat(text string) []MDArray {
	var arrays []MDArray
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) >= 4 && f[1] == ":" && strings.HasPrefix(f[0], "md"):
			arrays = append(arrays, MDArray{Name: f[0], Level: f[3]})
		case len(arrays) == 0:
		case strings.Contains(line, "blocks"):
			a := &arrays[len(arrays)-1]
			for _, w := range f {
				if total, active, ok := strings.Cut(strings.Trim(w, "[]"), "/"); ok && strings.HasPrefix(w, "[") {
					a.Total, _ = strconv.Atoi(total)
					a.Active, _ = strconv.Atoi(active)
				} else if strings.HasPrefix(w, "[") && strings.HasSuffix(w, "]") {
					a.Members = w
				}
			}
		case strings.Contains(line, "%"):
			for i, w := range f {
				if i+2 < len(f) && f[i+1] == "=" && strings.HasSuffix(f[i+2], "%") {
					arrays[len(arrays)-1].Sync = w + " " + f[i+2]
				}
			}
		}
	}
	return arrays
}

// parseCPUInfo takes the first "model name" and counts processors.
func parseCPUInfo(text string) CPU {
	var c CPU
	for _, line := range strings.Split(text, "\n") {
		k, v, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(k) {
		case "model name":
			if c.Model == "" {
				c.Model = strings.TrimSpace(v)
			}
		case "processor":
			c.Threads++
		}
	}
	return c
}

// parseMemInfo reads MemTotal and MemAvailable (kB) into bytes.
func parseMemInfo(text string) Memory {
	var m Memory
	for _, line := range strings.Split(text, "\n") {
		k, v, _ := strings.Cut(line, ":")
		kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		switch k {
		case "MemTotal":
			m.Total = kb * 1024
		case "MemAvailable":
			m.Available = kb * 1024
		}
	}
	return m
}

// parseUptime reads the first field of /proc/uptime (seconds).
func parseUptime(text string) time.Duration {
	secs, _ := strconv.ParseFloat(strings.Fields(text + " 0")[0], 64)
	return time.Duration(secs) * time.Second
}

// humanBytes formats n in binary units with one decimal above KiB.
func humanBytes(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	v, u := float64(n), 0
	for v >= 1024 && u < len(units)-1 {
		v /= 1024
		u++
	}
	if u == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", v, units[u])
}

// humanDuration formats an uptime as "1d 2h 3m".
func humanDuration(d time.Duration) string {
	days, hours, mins := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}
