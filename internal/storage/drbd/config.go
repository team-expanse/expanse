package drbd

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	experrors "github.com/expanse/expanse/internal/errors"
)

const (
	maxNodeID = 31 // DRBD 9 supports node-ids 0..31
	// MaxNodeID is exported for callers that validate ids before rendering.
	MaxNodeID = maxNodeID
	maxMinor  = 1<<20 - 1
	minPort   = 1024
	maxPort   = 65535
	// A majority quorum is only meaningful with three or more replicas: with
	// two, losing either one would stop the survivor.
	minQuorumReplicas = 3
)

var (
	hostName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	devicePath = regexp.MustCompile(`^/[A-Za-z0-9_./+-]+$`)
)

// Member is one replica: the host it lives on, its node-id and mesh address.
type Member struct {
	Host    string
	NodeID  int
	Address netip.Addr
}

// Resource is everything needed to render one /etc/drbd.d/<name>.res file.
type Resource struct {
	Name  string
	Minor int
	Port  int
	Disk  string // backing block device, e.g. /dev/vg0/<lv>
	// SplitBrainCmd is run by the kernel when it drops a split-brained
	// connection. It must only record the event; it never resolves it.
	SplitBrainCmd string
	Members       []Member
}

// Render returns the resource file. Output is deterministic: members are
// ordered by node-id. Split-brain is never resolved automatically.
func (r Resource) Render() (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	ms := append([]Member(nil), r.Members...)
	sort.Slice(ms, func(i, j int) bool { return ms[i].NodeID < ms[j].NodeID })

	var b strings.Builder
	fmt.Fprintf(&b, "resource %s {\n", r.Name)
	fmt.Fprintf(&b, "  device /dev/drbd%d minor %d;\n  disk %s;\n  meta-disk internal;\n", r.Minor, r.Minor, r.Disk)
	b.WriteString("  net {\n    protocol C;\n    verify-alg sha1;\n")
	b.WriteString("    after-sb-0pri disconnect;\n    after-sb-1pri disconnect;\n    after-sb-2pri disconnect;\n")
	b.WriteString("    rr-conflict disconnect;\n  }\n")
	b.WriteString(optionsBlock(len(ms)))
	if r.SplitBrainCmd != "" {
		fmt.Fprintf(&b, "  handlers {\n    split-brain %q;\n  }\n", r.SplitBrainCmd)
	}
	for _, m := range ms {
		fmt.Fprintf(&b, "  on %s { node-id %d; address %s; }\n", m.Host, m.NodeID, formatAddr(m.Address, r.Port))
	}
	if len(ms) > 1 {
		hosts := make([]string, len(ms))
		for i, m := range ms {
			hosts[i] = m.Host
		}
		fmt.Fprintf(&b, "  connection-mesh { hosts %s; }\n", strings.Join(hosts, " "))
	}
	b.WriteString("}\n")
	return b.String(), nil
}

func optionsBlock(replicas int) string {
	if replicas < minQuorumReplicas {
		return "  options {\n    quorum off;\n  }\n"
	}
	return "  options {\n    quorum majority;\n    on-no-quorum io-error;\n  }\n"
}

func formatAddr(a netip.Addr, port int) string {
	if a.Is6() {
		return fmt.Sprintf("ipv6 [%s]:%d", a, port)
	}
	return fmt.Sprintf("%s:%d", a, port)
}

func (r Resource) validate() error {
	const op = "drbd.Resource.Render"
	invalid := func(format string, args ...any) error {
		return experrors.New(experrors.KindInvalid, op, fmt.Sprintf("resource %q: "+format, append([]any{r.Name}, args...)...))
	}
	switch {
	case !resourceName.MatchString(r.Name):
		return invalid("invalid resource name")
	case r.Minor < 0 || r.Minor > maxMinor:
		return invalid("minor %d outside 0..%d", r.Minor, maxMinor)
	case r.Port < minPort || r.Port > maxPort:
		return invalid("port %d outside %d..%d", r.Port, minPort, maxPort)
	case !devicePath.MatchString(r.Disk):
		return invalid("disk %q is not an absolute device path", r.Disk)
	case strings.ContainsAny(r.SplitBrainCmd, "\"\\\r\n"):
		return invalid("split-brain command contains a quote, backslash or newline")
	case len(r.Members) == 0:
		return invalid("no members")
	}
	return r.validateMembers(invalid)
}

func (r Resource) validateMembers(invalid func(string, ...any) error) error {
	ids, hosts := map[int]bool{}, map[string]bool{}
	for _, m := range r.Members {
		switch {
		case m.NodeID < 0 || m.NodeID > maxNodeID:
			return invalid("node-id %d outside 0..%d", m.NodeID, maxNodeID)
		case ids[m.NodeID]:
			return invalid("duplicate node-id %d", m.NodeID)
		case !hostName.MatchString(m.Host):
			return invalid("invalid host name %q", m.Host)
		case hosts[m.Host]:
			return invalid("duplicate host %q", m.Host)
		case !m.Address.IsValid():
			return invalid("host %q has no address", m.Host)
		}
		ids[m.NodeID], hosts[m.Host] = true, true
	}
	return nil
}
