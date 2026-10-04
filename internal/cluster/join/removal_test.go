package join_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// peerTLS trusts the env's cluster CA, as a node dialing its peers does.
func (e *env) peerTLS() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(e.clusterCA.Cert)
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
}

// deadAddr is an address nothing listens on.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// fakeNoQuorum is a peer that cannot make a linearizable read.
type fakeNoQuorum struct {
	*raftstore.Store // nil: only Get is called
}

func (f *fakeNoQuorum) Get(context.Context, store.Key) (*store.Entry, error) {
	return nil, errors.New(errors.KindUnavailable, "fake", "no quorum")
}
func (f *fakeNoQuorum) IsLeader() bool             { return false }
func (f *fakeNoQuorum) AddVoter(_, _ string) error { return nil }
func (f *fakeNoQuorum) Leader() string             { return "" }

// serveNoQuorum runs a join endpoint whose node has lost quorum.
func (e *env) serveNoQuorum(t *testing.T) string {
	t.Helper()
	cert, priv, err := e.clusterCA.IssueNode("n9", "localhost", []net.IP{net.IPv4(127, 0, 0, 1)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tlsCert := tls.Certificate{Certificate: [][]byte{cert.Raw, e.clusterCA.Cert.Raw}, PrivateKey: priv}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{tlsCert}})))
	(&join.Service{St: &fakeNoQuorum{}, CA: e.clusterCA, ClusterID: testClusterID, Secret: e.secret}).Register(srv)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

func TestAPeerTellsARemovedNodeItWasRemoved(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rev := []byte(`{"id":"n5","removed_at":1,"by":"n0","reason":"retired"}`)
	if _, err := e.store.Put(ctx, store.Key(join.RevokedKeyPrefix+"n5"), rev); err != nil {
		t.Fatal(err)
	}
	got, err := join.AskRemoval(ctx, []string{e.joinAddr}, "n5", e.peerTLS())
	if err != nil {
		t.Fatalf("AskRemoval: %v", err)
	}
	if string(got) != string(rev) {
		t.Errorf("revocation = %s, want %s", got, rev)
	}
}

func TestAPeerTellsAMemberItWasNotRemoved(t *testing.T) {
	e := newEnv(t)
	got, err := join.AskRemoval(context.Background(), []string{e.joinAddr}, "n5", e.peerTLS())
	if err != nil || got != nil {
		t.Errorf("AskRemoval = %s, %v; want no revocation", got, err)
	}
}

func TestAskingForARemovalSkipsPeersThatCannotAnswer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.store.Put(ctx, store.Key(join.RevokedKeyPrefix+"n5"), []byte(`{"id":"n5"}`)); err != nil {
		t.Fatal(err)
	}
	peers := []string{deadAddr(t), e.serveNoQuorum(t), e.joinAddr}
	if got, err := join.AskRemoval(ctx, peers, "n5", e.peerTLS()); err != nil || got == nil {
		t.Errorf("AskRemoval = %s, %v; want the revocation from the peer with quorum", got, err)
	}
}

func TestAskingForARemovalFailsWhenNoPeerCanAnswer(t *testing.T) {
	e := newEnv(t)
	peers := []string{deadAddr(t), e.serveNoQuorum(t)}
	if got, err := join.AskRemoval(context.Background(), peers, "n5", e.peerTLS()); err == nil {
		t.Errorf("AskRemoval = %s, nil; want an error: no peer could answer", got)
	}
}

func TestAskingForARemovalTrustsOnlyTheClusterCA(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.store.Put(ctx, store.Key(join.RevokedKeyPrefix+"n5"), []byte(`{"id":"n5"}`)); err != nil {
		t.Fatal(err)
	}
	stranger := &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS13}
	if got, err := join.AskRemoval(ctx, []string{e.joinAddr}, "n5", stranger); err == nil {
		t.Errorf("AskRemoval = %s, nil; a peer outside the cluster CA must not be believed", got)
	}
}
