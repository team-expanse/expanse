package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/expanse/expanse/proto"
)

// fakeConsole greets each client, records its input and ends the session on "bye".
type fakeConsole struct {
	pb.UnimplementedConsoleServiceServer
	mu    sync.Mutex
	first *pb.ConsoleInput
	input bytes.Buffer
	fail  error
}

func (f *fakeConsole) Attach(stream pb.ConsoleService_AttachServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.first = first
	f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if err := stream.Send(&pb.ConsoleOutput{Data: []byte("login: ")}); err != nil {
		return err
	}
	for {
		in, err := stream.Recv()
		if err != nil {
			return nil //nolint:nilerr // the client hung up
		}
		f.mu.Lock()
		f.input.Write(in.GetData())
		bye := strings.Contains(f.input.String(), "bye")
		f.mu.Unlock()
		if bye {
			return nil
		}
	}
}

func (f *fakeConsole) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.input.String()
}

func consoleClient(t *testing.T, f *fakeConsole) pb.ConsoleServiceClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterConsoleServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewConsoleServiceClient(conn)
}

// syncWriter lets the test read output the console goroutine writes.
type syncWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConsoleRelaysUntilTheEscapeKey(t *testing.T) {
	f := &fakeConsole{}
	in, w := io.Pipe()
	out := &syncWriter{}
	done := make(chan error, 1)
	go func() {
		done <- attachConsole(context.Background(), consoleClient(t, f), &pb.ConsoleInput{Namespace: "apps", Name: "vm1"}, in, out, true)
	}()
	waitFor(t, "the greeting", func() bool { return out.String() == "login: " })
	_, _ = w.Write([]byte("root\r"))
	waitFor(t, "the input", func() bool { return f.got() == "root\r" })
	_, _ = w.Write([]byte("ls\x1dnot sent"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attachConsole: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ctrl-] did not end the console")
	}
	waitFor(t, "the text before Ctrl-]", func() bool { return f.got() == "root\rls" })
	if f.first.GetNamespace() != "apps" || f.first.GetName() != "vm1" {
		t.Errorf("first message %v, want apps/vm1", f.first)
	}
}

func TestPipedConsoleEndsWhenTheSessionDoes(t *testing.T) {
	f := &fakeConsole{}
	out := &syncWriter{}
	start := time.Now()
	err := attachConsole(context.Background(), consoleClient(t, f), &pb.ConsoleInput{Name: "vm1"}, strings.NewReader("bye\r"), out, false)
	if err != nil {
		t.Fatalf("attachConsole: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= pipedLinger {
		t.Errorf("took %v; the session ended, so it should not linger", elapsed)
	}
	if f.got() != "bye\r" || out.String() != "login: " {
		t.Errorf("sent %q and printed %q", f.got(), out.String())
	}
}

func TestConsoleReportsTheServersError(t *testing.T) {
	f := &fakeConsole{fail: status.Error(codes.FailedPrecondition, "web is a web/nginx block")}
	in, _ := io.Pipe()
	err := attachConsole(context.Background(), consoleClient(t, f), &pb.ConsoleInput{Name: "web"}, in, io.Discard, true)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("attachConsole error %v, want FailedPrecondition", err)
	}
}

// Input that ends at once must not hide why the console could not open.
func TestPipedConsoleReportsTheServersError(t *testing.T) {
	f := &fakeConsole{fail: status.Error(codes.NotFound, "block default/nosuch: not found")}
	err := attachConsole(context.Background(), consoleClient(t, f), &pb.ConsoleInput{Name: "nosuch"}, strings.NewReader(""), io.Discard, false)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("attachConsole error %v, want NotFound", err)
	}
}
