package chaos

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/expanse/expanse/proto"
)

// stubNode implements NodeService.GetLease with a fixed holder record —
// the injected double-hold for the checker's negative test.
type stubNode struct {
	pb.UnimplementedNodeServiceServer
	holder  string
	expires time.Time
	rev     int64
}

func (s *stubNode) GetLease(ctx context.Context, req *pb.GetLeaseRequest) (*pb.LeaseInfo, error) {
	return &pb.LeaseInfo{
		Found:           true,
		Holder:          s.holder,
		ExpiresAtUnixNs: s.expires.UnixNano(),
		Revision:        s.rev,
	}, nil
}

func serveLeaseStub(t *testing.T, holder string) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("stub listen: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(insecure.NewCredentials()))
	pb.RegisterNodeServiceServer(srv, &stubNode{
		holder:  holder,
		expires: time.Now().Add(time.Hour),
		rev:     7,
	})
	go srv.Serve(ln) //nolint:errcheck — test server
	return ln.Addr().String(), srv.Stop
}
