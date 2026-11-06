package console

import (
	"context"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	pb "github.com/expanse/expanse/proto"
)

// Locator turns a block lookup into Service.Locate: the node of a vm/instance replica's placement.
func Locator(get func(context.Context, *pb.GetBlockRequest) (*pb.Block, error)) func(context.Context, string, string, int32) (string, error) {
	return func(ctx context.Context, ns, name string, replica int32) (string, error) {
		b, err := get(ctx, &pb.GetBlockRequest{Namespace: ns, Name: name})
		if err != nil {
			return "", status.Errorf(codes.NotFound, "console: block %s/%s: %v", ns, name, err)
		}
		if t := b.GetSpec().GetType(); t != "vm/instance" {
			return "", status.Errorf(codes.FailedPrecondition, "console: %s/%s is a %s block; only VMs have a console", ns, name, t)
		}
		for _, p := range b.GetStatus().GetPlacements() {
			if p.GetReplicaIndex() == replica && p.GetNodeId() != "" {
				return p.GetNodeId(), nil
			}
		}
		return "", status.Errorf(codes.Unavailable, "console: replica %d of %s/%s is not placed on a node", replica, ns, name)
	}
}

// NodeDialer opens other nodes' ConsoleService over their internal API port, one connection each.
type NodeDialer struct {
	// Addr is a node's internal API address.
	Addr  func(nodeID string) (string, error)
	Creds credentials.TransportCredentials

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

// Dial is Service.Dial.
func (d *NodeDialer) Dial(_ context.Context, nodeID string) (pb.ConsoleServiceClient, error) {
	addr, err := d.Addr(nodeID)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if conn, ok := d.conns[addr]; ok {
		return pb.NewConsoleServiceClient(conn), nil
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(d.Creds))
	if err != nil {
		return nil, err
	}
	if d.conns == nil {
		d.conns = map[string]*grpc.ClientConn{}
	}
	d.conns[addr] = conn
	return pb.NewConsoleServiceClient(conn), nil
}
