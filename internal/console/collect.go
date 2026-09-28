package console

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/install"
	"github.com/expanse/expanse/internal/version"
	pb "github.com/expanse/expanse/proto"
)

// Options say where Collect looks.
type Options struct {
	Socket  string // the agent's unix socket
	DataDir string // /persist/expanse: node-id and identity
	Timeout time.Duration
}

// The agent re-runs every health check on GetHealth (seconds on a busy node, and the
// store check writes through Raft), so the screen takes its verdict from the agent's
// 10 s heartbeat key and asks for the failing checks' names only this often.
const (
	checksEvery   = time.Minute
	checksTimeout = 30 * time.Second
)

// Collector gathers the screen's data, keeps one connection to the agent (gRPC redials
// it as needed) and caches the slow part, the check names.
type Collector struct {
	Options

	conn     *grpc.ClientConn
	mu       sync.Mutex
	failing  []string
	checksAt time.Time
	fetching bool
}

// agentClient is the slice of the agent API the console reads; a fake stands in for tests.
type agentClient interface {
	GetStatus(context.Context, *pb.GetStatusRequest, ...grpc.CallOption) (*pb.NodeStatus, error)
	GetKeyValue(context.Context, *pb.GetKeyValueRequest, ...grpc.CallOption) (*pb.GetKeyValueResponse, error)
	GetHealth(context.Context, *pb.GetHealthRequest, ...grpc.CallOption) (*pb.HealthReport, error)
	GetClusterStatus(context.Context, *pb.GetClusterStatusRequest, ...grpc.CallOption) (*pb.GetClusterStatusResponse, error)
}

// Collect gathers the screen's data. It never fails: whatever cannot be read is left
// empty (and the agent's absence is reported), so the screen degrades instead of dying.
func (c *Collector) Collect(ctx context.Context) Info {
	in := Info{Version: version.Get().Version, Now: time.Now()}
	in.Hostname, _ = os.Hostname()
	in.NodeID = nodeID(c.DataDir)
	in.Uptime = parseUptime(readFile("/proc/uptime"))
	in.CPU = parseCPUInfo(readFile("/proc/cpuinfo"))
	in.Memory = parseMemInfo(readFile("/proc/meminfo"))
	in.Arrays = parseMdstat(readFile("/proc/mdstat"))
	labelArrays(in.Arrays, mdLabels("/dev/md"))
	in.Disks = sysfsDisks("/sys/block")
	in.Addrs = LocalAddrs()
	if fs, ok := statfs("/persist"); ok {
		in.Filesystems = []Filesystem{fs}
	}
	if _, err := os.Stat(c.Socket); err != nil {
		in.Agent.Err = "agent socket " + c.Socket + " not available"
		return in
	}
	if c.conn == nil {
		conn, err := grpc.NewClient("unix://"+c.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			in.Agent.Err = err.Error()
			return in
		}
		c.conn = conn
	}
	c.collectAgent(ctx, pb.NewNodeServiceClient(c.conn), &in)
	return in
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// nodeID resolves the id the way the agent does: enrollment, then identity, then hostname.
func nodeID(dataDir string) string {
	if id := control.LoadNodeID(dataDir); id != "" {
		return id
	}
	if id, err := install.LoadIdentity(filepath.Join(dataDir, install.IdentityDir)); err == nil {
		return id.NodeID.String()
	}
	h, _ := os.Hostname()
	return h
}

// sysfsDisks lists whole disks (not loop, ram, md, dm, ...) with size and model from sysfs.
func sysfsDisks(sysBlock string) []Disk {
	entries, _ := os.ReadDir(sysBlock)
	var disks []Disk
	for _, e := range entries {
		name := e.Name()
		if !isPhysicalDisk(name) {
			continue
		}
		sectors, _ := strconv.ParseInt(strings.TrimSpace(readFile(filepath.Join(sysBlock, name, "size"))), 10, 64)
		model := strings.TrimSpace(readFile(filepath.Join(sysBlock, name, "device", "model")))
		disks = append(disks, Disk{Name: name, Size: sectors * 512, Model: model})
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Name < disks[j].Name })
	return disks
}

func isPhysicalDisk(name string) bool {
	for _, p := range []string{"sd", "vd", "nvme", "hd", "xvd", "mmcblk"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// mdLabels maps md device names to their /dev/md/<label> symlinks (esp -> ../md127).
func mdLabels(dir string) map[string]string {
	entries, _ := os.ReadDir(dir)
	labels := map[string]string{}
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil {
			labels[filepath.Base(target)] = e.Name()
		}
	}
	return labels
}

func labelArrays(arrays []MDArray, labels map[string]string) {
	for i := range arrays {
		arrays[i].Label = labels[arrays[i].Name]
	}
}

func statfs(mount string) (Filesystem, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mount, &st); err != nil || st.Blocks == 0 {
		return Filesystem{}, false
	}
	bs := int64(st.Bsize)
	return Filesystem{Mount: mount, Total: int64(st.Blocks) * bs, Free: int64(st.Bavail) * bs}, true
}

// collectAgent probes the agent with its cheapest call, then reads the heartbeat key and
// the cluster report (each bounded by Timeout); the check names come from the cache.
func (c *Collector) collectAgent(ctx context.Context, a agentClient, in *Info) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	if _, err := a.GetStatus(ctx, &pb.GetStatusRequest{}); err != nil {
		in.Agent = AgentState{Up: status.Code(err) != codes.Unavailable, Err: err.Error()}
		return
	}
	in.Agent.Up = true
	res, err := a.GetKeyValue(ctx, &pb.GetKeyValueRequest{Key: "/nodes/" + in.NodeID + "/status", Stale: true})
	if err != nil {
		in.Agent.Err = err.Error()
	} else if res.GetFound() {
		in.Health = parseStatusValue(string(res.GetValue()))
	}
	c.mu.Lock()
	in.Health.Failing = c.failing
	stale := c.checksStale() && !c.fetching
	c.fetching = c.fetching || stale
	c.mu.Unlock()
	if stale {
		go c.refreshChecks(context.WithoutCancel(ctx), a)
	}
	in.Cluster = clusterFrom(ctx, a, in.NodeID)
}

// parseStatusValue reads the agent's heartbeat: "health=healthy [degraded=true writable=false]".
func parseStatusValue(v string) Health {
	var h Health
	for _, f := range strings.Fields(v) {
		switch k, val, _ := strings.Cut(f, "="); k {
		case "health":
			h.Overall = val
		case "degraded":
			h.NoQuorum = val == "true"
		}
	}
	return h
}

func (c *Collector) checksStale() bool { return time.Since(c.checksAt) > checksEvery }

// refreshChecks runs GetHealth once and caches the names of degraded or unhealthy checks
// (an unknown check is not a failure); on error the previous names stay until the next interval.
func (c *Collector) refreshChecks(ctx context.Context, a agentClient) {
	ctx, cancel := context.WithTimeout(ctx, checksTimeout)
	defer cancel()
	rep, err := a.GetHealth(ctx, &pb.GetHealthRequest{})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetching, c.checksAt = false, time.Now()
	if err != nil {
		return
	}
	c.failing = nil
	for _, ch := range rep.GetChecks() {
		if s := ch.GetStatus(); s == pb.Health_HEALTH_DEGRADED || s == pb.Health_HEALTH_UNHEALTHY {
			c.failing = append(c.failing, ch.GetName())
		}
	}
}

// clusterFrom decodes the cluster report; an agent outside a cluster refuses the call
// (FailedPrecondition), which reads as "not in a cluster yet".
func clusterFrom(ctx context.Context, a agentClient, self string) *Cluster {
	res, err := a.GetClusterStatus(ctx, &pb.GetClusterStatusRequest{})
	if err != nil {
		return nil
	}
	var rep control.Report
	if err := json.Unmarshal(res.GetReportJson(), &rep); err != nil {
		return nil
	}
	cl := &Cluster{Name: rep.Name, QuorumHave: rep.QuorumHave, QuorumNeed: rep.QuorumNeed, Nodes: len(rep.Nodes), Degraded: rep.Degraded}
	for _, n := range rep.Nodes {
		if n.ID == self {
			cl.Role = n.State
		}
	}
	return cl
}
