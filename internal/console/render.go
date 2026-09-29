package console

import (
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/expanse/expanse/internal/tuikit"
)

const (
	labelWidth = 10
	maxAddrs   = 4 // address rows before "+N more"
	maxArrays  = 3
	webPort    = 8443
)

// Render draws the host information screen for a terminal of sz.
func Render(in Info, sz tuikit.Size, g tuikit.Glyphs) []string {
	f := tuikit.Frame{
		Header:      "EXPANSE " + in.Version,
		HeaderRight: in.Now.Format("2006-01-02 15:04:05 MST"),
		Title:       orUnknown(in.Hostname),
		Footer:      tuikit.Keys("Alt+F2", "for a login shell") + tuikit.Dim + "  (this screen is read-only)" + tuikit.Reset,
	}
	inner := f.Inner(sz)
	half := inner.Cols/2 - 1
	body := []string{
		pair(kv("Hostname", in.Hostname), kv("Uptime", humanDuration(in.Uptime)), half),
	}
	if shownNodeID(in) {
		body = append(body, kv("Node ID", in.NodeID))
	}
	body = append(body, kv("Health", healthText(in)), tuikit.Rule("Management"))
	body = append(body, addressRows(in.Addrs, half)...)
	body = append(body, tuikit.Rule("Cluster"))
	body = append(body, clusterRows(in, half)...)
	body = append(body, tuikit.Rule("Hardware"),
		kv("CPU", fmt.Sprintf("%s (%d threads)", orUnknown(in.CPU.Model), in.CPU.Threads)),
		kv("Memory", freeOf(in.Memory.Available, in.Memory.Total)),
		kv("Disks", diskText(in.Disks)))
	for i, a := range in.Arrays {
		if i == maxArrays {
			body = append(body, kv("", fmt.Sprintf("and %d more arrays", len(in.Arrays)-maxArrays)))
			break
		}
		body = append(body, kv("Mirror", arrayText(a)))
	}
	for _, fs := range in.Filesystems {
		body = append(body, kv(fs.Mount, freeOf(fs.Free, fs.Total)))
	}
	f.Body = body
	return f.Render(sz, g)
}

// shownNodeID: a clustered node's ID is its hostname, and a bare UUID is no help on a console.
func shownNodeID(in Info) bool {
	_, err := uuid.Parse(in.NodeID)
	return in.NodeID != "" && in.NodeID != in.Hostname && err != nil
}

func kv(label, value string) string { return tuikit.KV(label, orUnknown(value), labelWidth) }

// pair puts two "label value" cells side by side, each in w cells.
func pair(left, right string, w int) string { return tuikit.Pad(left, w) + "  " + right }

func orUnknown(s string) string {
	if s == "" {
		return tuikit.Dim + "unknown" + tuikit.Reset
	}
	return s
}

func freeOf(free, total int64) string {
	if total == 0 {
		return ""
	}
	return humanBytes(free) + " free of " + humanBytes(total)
}

// healthText is the agent's verdict in colour, or why there is none.
func healthText(in Info) string {
	if !in.Agent.Up {
		return tuikit.Red + "agent not running" + tuikit.Reset + tuikit.Dim + "  (systemctl status expansed)" + tuikit.Reset
	}
	if in.Health.Overall == "" {
		why := "waiting for the agent's first heartbeat"
		if in.Agent.Err != "" {
			why = "agent slow to answer"
		}
		return tuikit.Dim + "unknown  (" + why + ")" + tuikit.Reset
	}
	colour := map[string]string{"healthy": tuikit.Green, "degraded": tuikit.Yellow, "unhealthy": tuikit.Red}[in.Health.Overall]
	text := colour + tuikit.Bold + strings.ToUpper(in.Health.Overall) + tuikit.Reset
	if in.Health.Overall != "healthy" && len(in.Health.Failing) > 0 { // the names are cached; the verdict is current
		text += tuikit.Dim + "  " + strings.Join(in.Health.Failing, ", ") + tuikit.Reset
	}
	return text
}

// addressRows lists interface addresses, the first beside the web UI URL, capped at maxAddrs.
func addressRows(addrs []Addr, half int) []string {
	if len(addrs) == 0 {
		return []string{kv("Address", tuikit.Yellow+"none yet"+tuikit.Reset+tuikit.Dim+"  (waiting for DHCP)"+tuikit.Reset)}
	}
	var rows []string
	for i, a := range addrs {
		if i == maxAddrs {
			rows = append(rows, kv("", fmt.Sprintf("and %d more addresses", len(addrs)-maxAddrs)))
			break
		}
		row := kv(a.Iface, a.IP)
		if i == 0 {
			row = pair(row, kv("Web UI", tuikit.Cyan+webURL(a.IP)+tuikit.Reset), half)
		}
		rows = append(rows, row)
	}
	return rows
}

// webURL is the UI's address for ip, bracketing IPv6.
func webURL(ip string) string {
	if strings.Contains(ip, ":") {
		ip = "[" + ip + "]"
	}
	return fmt.Sprintf("https://%s:%d", ip, webPort)
}

// clusterRows describes membership, or how to form a cluster when there is none.
func clusterRows(in Info, half int) []string {
	c := in.Cluster
	switch {
	case c != nil:
		quorum := fmt.Sprintf("%d/%d  (%s)", c.QuorumHave, c.QuorumNeed, plural(c.Nodes, "node"))
		if c.Degraded || in.Health.NoQuorum {
			quorum = tuikit.Yellow + fmt.Sprintf("%d/%d", c.QuorumHave, c.QuorumNeed) + "  NO LEADER, read-only" + tuikit.Reset
		}
		return []string{pair(kv("Cluster", c.Name), kv("Role", c.Role), half), kv("Quorum", quorum)}
	case !in.Agent.Up:
		return []string{kv("Cluster", tuikit.Dim+"unknown while the agent is down"+tuikit.Reset)}
	}
	return []string{
		kv("Cluster", tuikit.Yellow+"not in a cluster yet"+tuikit.Reset+tuikit.Dim+"  -- to form one, with the agent stopped:"+tuikit.Reset),
		kv("", tuikit.Cyan+"systemctl stop expansed && expanse cluster init --expect 1 \\"+tuikit.Reset),
		kv("", tuikit.Cyan+"  && systemctl start expansed"+tuikit.Reset),
	}
}

func diskText(disks []Disk) string {
	var parts []string
	for _, d := range disks {
		parts = append(parts, strings.TrimSpace(fmt.Sprintf("%s %s %s", d.Name, humanBytes(d.Size), d.Model)))
	}
	return strings.Join(parts, ",  ")
}

// arrayText is "system  md126 raid1 [UU]" with a red DEGRADED and any resync progress.
func arrayText(a MDArray) string {
	s := fmt.Sprintf("%s %s %s", a.Name, a.Level, a.Members)
	if a.Label != "" {
		s = tuikit.Bold + a.Label + tuikit.Reset + "  " + s
	}
	if a.Degraded() {
		s = tuikit.Red + tuikit.Bold + s + "  DEGRADED" + tuikit.Reset
	} else {
		s = tuikit.Green + s + tuikit.Reset
	}
	if a.Sync != "" {
		s += tuikit.Yellow + "  " + a.Sync + tuikit.Reset
	}
	return s
}

// plural renders "1 node" / "3 nodes".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
