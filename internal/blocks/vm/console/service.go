package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	pb "github.com/expanse/expanse/proto"
)

// Service serves ConsoleService: a replica on this node is attached to its console socket, and
// one elsewhere is forwarded to the node that runs it.
type Service struct {
	pb.UnimplementedConsoleServiceServer

	NodeID string
	// Locate names the node running a replica, or returns a gRPC status error.
	Locate func(ctx context.Context, ns, name string, replica int32) (string, error)
	// Dial opens another node's ConsoleService.
	Dial func(ctx context.Context, nodeID string) (pb.ConsoleServiceClient, error)
	// Socket is a local replica's console socket; nil means its systemd instance's.
	Socket func(ns, name string, replica int32) string
	// Done ends every open console when closed, so a server's graceful stop isn't held up.
	Done <-chan struct{}
}

// Attach relays one console session; the first message names the replica.
func (s *Service) Attach(stream pb.ConsoleService_AttachServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	ns, name, replica := first.GetNamespace(), first.GetName(), first.GetReplica()
	if ns == "" {
		ns = "default"
	}
	if name == "" {
		return status.Error(codes.InvalidArgument, "console: no block name")
	}
	host, err := s.Locate(stream.Context(), ns, name, replica)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	go func() {
		select {
		case <-s.Done:
			cancel()
		case <-ctx.Done():
		}
	}()
	if host == s.NodeID {
		err = s.local(ctx, stream, s.socket(ns, name, replica), first.GetData())
	} else if first.GetForwarded() {
		return status.Errorf(codes.FailedPrecondition, "console: replica %d of %s/%s runs on %s, not here", replica, ns, name, host)
	} else {
		err = s.forward(ctx, stream, host, &pb.ConsoleInput{
			Namespace: ns, Name: name, Replica: replica, Data: first.GetData(), Forwarded: true,
		})
	}
	if s.closing() {
		return status.Error(codes.Unavailable, "console: the node is shutting down")
	}
	return err
}

func (s *Service) socket(ns, name string, replica int32) string {
	if s.Socket != nil {
		return s.Socket(ns, name, replica)
	}
	return SocketPath(systemd.Instance(ns, name, int(replica)))
}

func (s *Service) closing() bool {
	select {
	case <-s.Done:
		return true
	default:
		return false
	}
}

// local relays stream to and from the console socket at path until either side ends.
func (s *Service) local(ctx context.Context, stream pb.ConsoleService_AttachServer, path string, data []byte) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return status.Errorf(codes.Unavailable, "console: the VM's console is not up on %s: %v", s.NodeID, err)
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	if len(data) > 0 {
		if _, err := conn.Write(data); err != nil {
			return status.Errorf(codes.Unavailable, "console: %v", err)
		}
	}
	ended := make(chan struct{}, 2)
	go func() {
		for {
			in, err := stream.Recv()
			if err != nil {
				ended <- struct{}{} // the client went away
				return
			}
			if _, err := conn.Write(in.GetData()); err != nil {
				ended <- struct{}{}
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if serr := stream.Send(&pb.ConsoleOutput{Data: append([]byte(nil), buf[:n]...)}); serr != nil {
					ended <- struct{}{}
					return
				}
			}
			if err != nil {
				ended <- struct{}{} // the VM stopped
				return
			}
		}
	}()
	<-ended
	return nil
}

// forward relays stream through host's ConsoleService, opening it with first.
func (s *Service) forward(ctx context.Context, stream pb.ConsoleService_AttachServer, host string, first *pb.ConsoleInput) error {
	client, err := s.Dial(ctx, host)
	if err != nil {
		return status.Errorf(codes.Unavailable, "console: reach %s: %v", host, err)
	}
	up, err := client.Attach(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "console: reach %s: %v", host, err)
	}
	if err := up.Send(first); err != nil {
		return status.Errorf(codes.Unavailable, "console: reach %s: %v", host, err)
	}
	go func() {
		for {
			in, err := stream.Recv()
			if err != nil {
				_ = up.CloseSend()
				return
			}
			if err := up.Send(&pb.ConsoleInput{Data: in.GetData()}); err != nil {
				return
			}
		}
	}()
	for {
		out, err := up.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if _, ok := status.FromError(err); ok {
				return err
			}
			return fmt.Errorf("console: %s: %w", host, err)
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
}
