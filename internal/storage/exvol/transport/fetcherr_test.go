package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	expb "github.com/expanse/expanse/proto"
)

func fetchServer(t *testing.T, h func(string, uint64, uint64) (*expb.FetchOpsReply, error)) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(ln, func(string) (Handler, error) { return nil, errors.New("no writes here") }, nil, 0)
	srv.SetFetchHandler(h)
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { srv.Close() })
	return srv, ln.Addr().String()
}

// A peer that ANSWERS with an error is up and unable to serve: a
// definitive reply, kept apart from a connection that failed.
func TestFetchOpsHandlerErrorIsReplicaError(t *testing.T) {
	_, addr := fetchServer(t, func(string, uint64, uint64) (*expb.FetchOpsReply, error) {
		return nil, errors.New("op re-read failed")
	})
	c, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.FetchOps("v", 0, 1)
	var re *ReplicaError
	if !errors.As(err, &re) || re.Msg != "op re-read failed" {
		t.Fatalf("err = %v, want a *ReplicaError carrying the handler's message", err)
	}
	if IsConnFailure(err) {
		t.Fatal("an answered error is not a connection failure")
	}
	if _, err = c.FetchOps("v", 0, 1); !errors.As(err, &re) {
		t.Fatalf("the connection must stay usable after an error reply, got %v", err)
	}
}

func TestFetchOpsOnDeadConnectionIsConnFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { // a peer that dies as soon as it is connected to
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	c, err := Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.FetchOps("v", 0, 1)
	if err == nil {
		t.Fatal("fetch over a dead connection must fail")
	}
	var re *ReplicaError
	if errors.As(err, &re) || !IsConnFailure(err) {
		t.Fatalf("err = %v: want a connection failure, not a ReplicaError", err)
	}
}

func TestIsConnFailureClassification(t *testing.T) {
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, net.ErrClosed, &net.OpError{Op: "read", Err: errors.New("reset")}} {
		if !IsConnFailure(err) {
			t.Errorf("%v should be a connection failure", err)
		}
	}
	for _, err := range []error{nil, errors.New("crc mismatch"), &ReplicaError{Msg: "no such op"}} {
		if IsConnFailure(err) {
			t.Errorf("%v must not be a connection failure", err)
		}
	}
}
