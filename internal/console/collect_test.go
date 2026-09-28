package console

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/expanse/expanse/proto"
)

// fakeAgent answers the three RPCs the console uses, with configurable errors.
type fakeAgent struct {
	statusErr   error
	statusValue string
	healthErr   error
	healthCalls int
	clusterErr  error
}

func (f *fakeAgent) GetStatus(context.Context, *pb.GetStatusRequest, ...grpc.CallOption) (*pb.NodeStatus, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &pb.NodeStatus{NodeId: "n1"}, nil
}

func (f *fakeAgent) GetKeyValue(_ context.Context, req *pb.GetKeyValueRequest, _ ...grpc.CallOption) (*pb.GetKeyValueResponse, error) {
	if f.statusValue == "" || req.GetKey() != "/nodes/n1/status" {
		return &pb.GetKeyValueResponse{Found: false}, nil
	}
	return &pb.GetKeyValueResponse{Found: true, Value: []byte(f.statusValue)}, nil
}

func (f *fakeAgent) GetHealth(context.Context, *pb.GetHealthRequest, ...grpc.CallOption) (*pb.HealthReport, error) {
	f.healthCalls++
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	return &pb.HealthReport{Overall: pb.HealthReport_OVERALL_DEGRADED, Checks: []*pb.CheckResult{
		{Name: "disk-space", Status: pb.Health_HEALTH_HEALTHY},
		{Name: "clock-sync", Status: pb.Health_HEALTH_UNHEALTHY},
		{Name: "store", Status: pb.Health_HEALTH_UNKNOWN},
	}}, nil
}

func (f *fakeAgent) GetClusterStatus(context.Context, *pb.GetClusterStatusRequest, ...grpc.CallOption) (*pb.GetClusterStatusResponse, error) {
	if f.clusterErr != nil {
		return nil, f.clusterErr
	}
	return &pb.GetClusterStatusResponse{ReportJson: []byte(`{"Name":"lab","QuorumHave":1,"QuorumNeed":1,"Leader":"10.0.0.1:7444",
		"Nodes":[{"ID":"n1","RaftAddr":"10.0.0.1:7444","Role":"voter","State":"leader"}]}`)}, nil
}

func TestParseStatusValue(t *testing.T) {
	h := parseStatusValue("health=degraded degraded=true writable=false")
	if h.Overall != "degraded" || !h.NoQuorum {
		t.Errorf("parsed %+v", h)
	}
	if h := parseStatusValue("health=healthy"); h.Overall != "healthy" || h.NoQuorum {
		t.Errorf("parsed %+v", h)
	}
}

func TestHeartbeatGivesHealthWithoutRunningChecks(t *testing.T) {
	f := &fakeAgent{statusValue: "health=healthy", clusterErr: status.Error(codes.FailedPrecondition, "not a cluster-mode agent")}
	c := &Collector{Options: Options{Timeout: time.Second}}
	var in Info
	in.NodeID = "n1"
	c.collectAgent(context.Background(), f, &in)
	if !in.Agent.Up || in.Agent.Err != "" || in.Health.Overall != "healthy" || in.Cluster != nil {
		t.Fatalf("agent %+v health %+v cluster %+v", in.Agent, in.Health, in.Cluster)
	}
	if f.healthCalls != 0 {
		t.Fatalf("GetHealth called %d times from the refresh path; checks must come from the cache", f.healthCalls)
	}
}

func TestFailingChecksAreFetchedAtMostOncePerMinute(t *testing.T) {
	f := &fakeAgent{statusValue: "health=degraded"}
	c := &Collector{Options: Options{Timeout: time.Second}}
	if !c.checksStale() {
		t.Fatal("a fresh collector must want the checks")
	}
	c.refreshChecks(context.Background(), f)
	if c.checksStale() || f.healthCalls != 1 {
		t.Fatalf("stale=%v calls=%d after one refresh", c.checksStale(), f.healthCalls)
	}
	var in Info
	in.NodeID = "n1"
	c.collectAgent(context.Background(), f, &in)
	if want := []string{"clock-sync"}; !reflect.DeepEqual(in.Health.Failing, want) { // "store" is unknown, not failing
		t.Fatalf("failing = %v, want %v", in.Health.Failing, want)
	}
	c.checksAt = time.Now().Add(-2 * time.Minute)
	if !c.checksStale() {
		t.Fatal("checks older than a minute must be refreshed")
	}
}

func TestAgentDownVersusSlow(t *testing.T) {
	down := &fakeAgent{statusErr: status.Error(codes.Unavailable, "connection refused")}
	c := &Collector{Options: Options{Timeout: time.Second}}
	var in Info
	c.collectAgent(context.Background(), down, &in)
	if in.Agent.Up || in.Agent.Err == "" {
		t.Fatalf("an unavailable agent should read as down: %+v", in.Agent)
	}
	slow := &fakeAgent{statusErr: status.Error(codes.DeadlineExceeded, "context deadline exceeded")}
	in = Info{}
	c.collectAgent(context.Background(), slow, &in)
	if !in.Agent.Up || in.Agent.Err == "" {
		t.Fatalf("a slow agent must not read as down: %+v", in.Agent)
	}
	c.checksAt = time.Now()
	c.failing = []string{"clock-sync"}
	stale := &fakeAgent{statusValue: "health=healthy", healthErr: status.Error(codes.DeadlineExceeded, "slow")}
	c.refreshChecks(context.Background(), stale)
	if c.failing == nil {
		t.Fatal("a failed refresh must keep the cached check names")
	}
}

func TestMDLabelsFromDevMdSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("../md126", filepath.Join(dir, "system")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../md127", filepath.Join(dir, "esp")); err != nil {
		t.Fatal(err)
	}
	arrays := []MDArray{{Name: "md126"}, {Name: "md127"}, {Name: "md0"}}
	labelArrays(arrays, mdLabels(dir))
	if arrays[0].Label != "system" || arrays[1].Label != "esp" || arrays[2].Label != "" {
		t.Fatalf("labels: %+v", arrays)
	}
}
