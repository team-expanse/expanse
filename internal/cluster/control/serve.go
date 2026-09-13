package control

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/config"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// ServeJoinEndpoint runs the leader-side JoinService on :7446 (TLS 1.3
// with this node's cert) until ctx is canceled. Safe on every node: the
// service itself redirects non-leaders with a leader_join hint.
func ServeJoinEndpoint(ctx context.Context, st *raftstore.Store, clusterCA *ca.CA, secret []byte, dataDir, clusterID string) error {
	certPEM, err := os.ReadFile(filepath.Join(dataDir, NodeCertFile))
	if err != nil {
		return errors.New(errors.KindNotFound, "control.ServeJoinEndpoint", "node cert missing: "+err.Error())
	}
	keyPEM, err := os.ReadFile(filepath.Join(dataDir, NodeKeyFile))
	if err != nil {
		return errors.New(errors.KindNotFound, "control.ServeJoinEndpoint", "node key missing: "+err.Error())
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return errors.New(errors.KindInternal, "control.ServeJoinEndpoint", "bad node keypair: "+err.Error())
	}
	bundle, err := ca.NewBundle(clusterCA.Cert)
	if err != nil {
		return err
	}
	svc := &join.Service{
		St: st, CA: clusterCA, ClusterID: clusterID, Secret: secret, ThisNodeID: st.NodeID(),
		LeaderJoinAddr: func(raftAddr string) (string, bool) {
			host, _, err := net.SplitHostPort(raftAddr)
			if err != nil || host == "" {
				return "", false
			}
			return net.JoinHostPort(host, fmt.Sprintf("%d", config.PortJoin)), true
		},
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", config.PortJoin))
	if err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "control.ServeJoinEndpoint", "bind :7446: "+err.Error())
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(join.ServerTLSConfig(pair, bundle))))
	svc.Register(srv)
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
		_ = ln.Close()
	}()
	return srv.Serve(ln)
}

var _ = config.PortRaft

// InternalTLS builds the SERVER-side mutual-TLS config for the
// node↔node gRPC endpoint (:7443): the node's own cert, cluster-CA
// trust, and a live CN-membership check against the store (stale reads
// only — the check runs during handshakes a linearizable read itself
// would need).
func InternalTLS(ctx context.Context, st *raftstore.Store, clusterCA *ca.CA, dataDir string) (*tls.Config, error) {
	pair, bundle, err := internalIdentity(ctx, st, clusterCA, dataDir)
	if err != nil {
		return nil, err
	}
	knownNodes := membershipCheck(ctx, st)
	return ca.TLSConfig(bundle, pair, knownNodes), nil
}

// InternalClientTLS is the CLIENT-side config for dialing peer internal
// endpoints: same identity and CA chain, but verification is
// CN-membership based (no IP/hostname matching — node certs carry no IP
// SANs and peers dial by whatever address the cluster view has).
func InternalClientTLS(ctx context.Context, st *raftstore.Store, clusterCA *ca.CA, dataDir string) (*tls.Config, error) {
	pair, bundle, err := internalIdentity(ctx, st, clusterCA, dataDir)
	if err != nil {
		return nil, err
	}
	knownNodes := membershipCheck(ctx, st)
	return ca.PeerTLSConfig(bundle, pair, knownNodes), nil
}

func internalIdentity(ctx context.Context, st *raftstore.Store, clusterCA *ca.CA, dataDir string) (tls.Certificate, *ca.Bundle, error) {
	certPEM, err := os.ReadFile(filepath.Join(dataDir, NodeCertFile))
	if err != nil {
		return tls.Certificate{}, nil, errors.New(errors.KindNotFound, "control.InternalTLS", "node cert missing: "+err.Error())
	}
	keyPEM, err := os.ReadFile(filepath.Join(dataDir, NodeKeyFile))
	if err != nil {
		return tls.Certificate{}, nil, errors.New(errors.KindNotFound, "control.InternalTLS", "node key missing: "+err.Error())
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, errors.New(errors.KindInternal, "control.InternalTLS", "bad node keypair: "+err.Error())
	}
	bundle, err := ca.NewBundle(clusterCA.Cert)
	if err != nil {
		return tls.Certificate{}, nil, errors.Wrap(err, errors.KindInternal, "control.InternalTLS", "trust bundle: "+err.Error())
	}
	return pair, bundle, nil
}

// membershipCheck is the live CN-membership callback from the store
// (stale reads only).
func membershipCheck(ctx context.Context, st *raftstore.Store) func() []string {
	return func() []string {
		entries, err := st.List(store.WithStale(ctx), store.Key(join.NodesKeyPrefix))
		if err != nil {
			return nil
		}
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, strings.TrimPrefix(string(e.Key), join.NodesKeyPrefix))
		}
		sort.Strings(ids)
		return ids
	}
}

// ServeInternalEndpoint runs the node↔node gRPC service on :7443 (§3
// fixed ports, G3.8 mTLS): it hosts the store's internal forwarding
// endpoint so followers can forward writes and linearizable reads to
// the leader. Peers must present a cluster-CA-signed cert whose CN is
// a current member (live membership check). Blocks until ctx is done.
func ServeInternalEndpoint(ctx context.Context, st *raftstore.Store, clusterCA *ca.CA, dataDir string) error {
	tlsCfg, err := InternalTLS(ctx, st, clusterCA, dataDir)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", config.PortAPI))
	if err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "control.ServeInternalEndpoint", "bind :7443: "+err.Error())
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	pb.RegisterInternalStoreServiceServer(srv, raftstore.NewForwardServer(st))
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
		_ = ln.Close()
	}()
	return srv.Serve(ln)
}
