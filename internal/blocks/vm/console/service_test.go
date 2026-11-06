package console

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/expanse/expanse/proto"
)

// node is one node's console service on a loopback gRPC server.
type node struct {
	svc  *Service
	addr string
}

// cluster runs a console service per node id; vm1 replica 0 runs on host, its guest echoing to guestIn.
type cluster struct {
	nodes   map[string]*node
	guestIn *syncBuffer
	guest   *io.PipeWriter
}

func newCluster(t *testing.T, host string, ids ...string) *cluster {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "vm1.sock")
	c := &cluster{nodes: map[string]*node{}, guestIn: &syncBuffer{}}
	mux := NewMux(c.guestIn, io.Discard)
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = mux.Serve(ln) }()
	out, w := io.Pipe()
	c.guest = w
	go func() { _ = mux.Pump(out) }()

	for _, id := range ids {
		c.nodes[id] = &node{svc: &Service{
			NodeID: id,
			Locate: func(_ context.Context, ns, name string, replica int32) (string, error) {
				if ns == "default" && name == "vm1" && replica == 0 {
					return host, nil
				}
				return "", status.Error(codes.NotFound, "no such replica")
			},
			Socket: func(string, string, int32) string { return sock },
			Dial:   c.dial,
		}}
	}
	for _, n := range c.nodes {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer()
		pb.RegisterConsoleServiceServer(srv, n.svc)
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		n.addr = lis.Addr().String()
	}
	return c
}

func (c *cluster) dial(_ context.Context, id string) (pb.ConsoleServiceClient, error) {
	n, ok := c.nodes[id]
	if !ok {
		return nil, status.Error(codes.Unavailable, "unknown node")
	}
	conn, err := grpc.NewClient(n.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return pb.NewConsoleServiceClient(conn), nil
}

// attach opens a console on node id for replica, sending first as the opening message.
func (c *cluster) attach(t *testing.T, id string, first *pb.ConsoleInput) pb.ConsoleService_AttachClient {
	t.Helper()
	client, err := c.dial(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := client.Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(first); err != nil {
		t.Fatal(err)
	}
	return stream
}

func recvUntil(t *testing.T, stream pb.ConsoleService_AttachClient, want string) {
	t.Helper()
	got := ""
	done := make(chan error, 1)
	go func() {
		for !strings.Contains(got, want) {
			out, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			got += string(out.GetData())
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("got %q, want %q: %v", got, want, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out with %q, want %q", got, want)
	}
}

func vm1() *pb.ConsoleInput { return &pb.ConsoleInput{Namespace: "default", Name: "vm1"} }

func TestAttachOnTheHostingNodeRelaysBothWays(t *testing.T) {
	c := newCluster(t, "n1", "n1")
	first := vm1()
	first.Data = []byte("root\r")
	stream := c.attach(t, "n1", first)
	_, _ = c.guest.Write([]byte("Password: "))
	recvUntil(t, stream, "Password: ")
	_ = stream.Send(&pb.ConsoleInput{Data: []byte("x\r")})
	eventually(t, "the input to reach the guest", func() bool { return c.guestIn.String() == "root\rx\r" })
}

func TestAttachOnAnotherNodeIsForwardedToTheHost(t *testing.T) {
	c := newCluster(t, "n2", "n1", "n2")
	stream := c.attach(t, "n1", vm1())
	_, _ = c.guest.Write([]byte("login: "))
	recvUntil(t, stream, "login: ")
	_ = stream.Send(&pb.ConsoleInput{Data: []byte("root\r")})
	eventually(t, "the input to reach the guest", func() bool { return c.guestIn.String() == "root\r" })
}

func wantCode(t *testing.T, stream pb.ConsoleService_AttachClient, want codes.Code) {
	t.Helper()
	_, err := stream.Recv()
	if status.Code(err) != want {
		t.Fatalf("Recv error %v, want %v", err, want)
	}
}

// A forwarded request reaching a node that doesn't host the replica (it moved) is refused, not routed on.
func TestAForwardedAttachIsNotForwardedAgain(t *testing.T) {
	c := newCluster(t, "n2", "n1", "n2")
	first := vm1()
	first.Forwarded = true
	wantCode(t, c.attach(t, "n1", first), codes.FailedPrecondition)
}

func TestAttachToAReplicaWithNoPlacementIsNotFound(t *testing.T) {
	c := newCluster(t, "n1", "n1")
	wantCode(t, c.attach(t, "n1", &pb.ConsoleInput{Namespace: "default", Name: "nope"}), codes.NotFound)
}

func TestAttachWithoutABlockNameIsInvalid(t *testing.T) {
	c := newCluster(t, "n1", "n1")
	wantCode(t, c.attach(t, "n1", &pb.ConsoleInput{}), codes.InvalidArgument)
}

func TestAttachWhenTheConsoleIsNotUpIsUnavailable(t *testing.T) {
	c := newCluster(t, "n1", "n1")
	c.nodes["n1"].svc.Socket = func(string, string, int32) string { return filepath.Join(t.TempDir(), "absent.sock") }
	wantCode(t, c.attach(t, "n1", vm1()), codes.Unavailable)
}

// Done ends every open console, so a server's graceful stop isn't held up by one.
func TestDoneEndsAnOpenConsole(t *testing.T) {
	c := newCluster(t, "n1", "n1")
	done := make(chan struct{})
	c.nodes["n1"].svc.Done = done
	stream := c.attach(t, "n1", vm1())
	_, _ = c.guest.Write([]byte("login: "))
	recvUntil(t, stream, "login: ")
	close(done)
	wantCode(t, stream, codes.Unavailable)
}
