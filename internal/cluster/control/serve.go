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

// ServeJoinEndpoint runs the leader-side JoinService on the fixed join
// port (config.PortJoin) until ctx is canceled. Safe on every node: the
// service itself redirects non-leaders with a leader_join hint.
func ServeJoinEndpoint(ctx context.Context, st *raftstore.Store, clusterCA *ca.CA, secret []byte, dataDir, clusterID string) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", config.PortJoin))
	if err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "control.ServeJoinEndpoint", "bind :7446: "+err.Error())
	}
	return ServeJoinEndpointOn(ctx, st, clusterCA, secret, dataDir, clusterID, ln)
}

// ServeJoinEndpointOn is ServeJoinEndpoint over a caller-provided
// listener (tests bind a free loopback port; production binds :7446).
func ServeJoinEndpointOn(ctx context.Context, st *raftstore.Store, clusterCA *ca.CA, secret []byte, dataDir, clusterID string, ln net.Listener) error {
	certSrc := diskCertSource(dataDir)
	if _, err := certSrc(); err != nil {
		return err
	}
	svc := &join.Service{
		St: st, CA: clusterCA, ClusterID: clusterID, Secret: secret, ThisNodeID: st.NodeID(),
		// CASource keeps new joiners signing under the live primary CA
		// (Phase 10 X2): without it, a leader that has served since
		// before a rotation would keep signing joiners with whatever CA
		// was loaded at startup, defeating the rotation for anyone who
		// joins mid-rotation.
		CASource: func() (*ca.CA, error) {
			trust, _, err := LoadCATrust(ctx, st)
			if err != nil {
				if errors.Is(err, errors.KindNotFound) {
					return clusterCA, nil
				}
				return nil, err
			}
			return trust.Primary.ca(secret)
		},
		LeaderJoinAddr: func(raftAddr string) (string, bool) {
			host, _, err := net.SplitHostPort(raftAddr)
			if err != nil || host == "" {
				return "", false
			}
			return net.JoinHostPort(host, fmt.Sprintf("%d", config.PortJoin)), true
		},
	}
	// ClientAuth is NoClientCert (joiners have no credentials yet — the
	// token authorizes), so only this node's own serving certificate
	// needs to be dynamic; it re-reads from disk on every handshake so
	// a renewal (control's renewal loop) takes effect without a
	// listener restart.
	tlsCfg := &tls.Config{
		MinVersion:     tls.VersionTLS13,
		ClientAuth:     tls.NoClientCert,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return certSrc() },
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
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
// would need). The identity certificate and trust bundle are both
// re-read fresh on every handshake (ca.TLSConfigDynamic), so cert
// renewal and CA rotation (Phase 10 X2) take effect without restarting
// the listener — mirrors VerifyPeerCN's existing no-restart design.
func InternalTLS(ctx context.Context, st *raftstore.Store, clusterCA *ca.CA, dataDir string) (*tls.Config, error) {
	certSrc := diskCertSource(dataDir)
	if _, err := certSrc(); err != nil {
		return nil, err
	}
	knownNodes := membershipCheck(ctx, st)
	bundleSrc := storeBundleSource(ctx, st, clusterCA)
	return ca.TLSConfigDynamic(bundleSrc, certSrc, knownNodes), nil
}

// InternalClientTLS is the CLIENT-side config for dialing peer internal
// endpoints: same identity and CA chain, but verification is
// CN-membership based (no IP/hostname matching — node certs carry no IP
// SANs and peers dial by whatever address the cluster view has).
func InternalClientTLS(ctx context.Context, st *raftstore.Store, clusterCA *ca.CA, dataDir string) (*tls.Config, error) {
	certSrc := diskCertSource(dataDir)
	if _, err := certSrc(); err != nil {
		return nil, err
	}
	knownNodes := membershipCheck(ctx, st)
	bundleSrc := storeBundleSource(ctx, st, clusterCA)
	return ca.PeerTLSConfigDynamic(bundleSrc, certSrc, knownNodes), nil
}

// UIServerTLS builds the SERVER-side TLS config for the web UI listener
// (config.PortUI, ROADMAP.md Phase 2 D5): the node's own cluster-CA-
// issued certificate, but — unlike InternalTLS — no client certificate
// is required and there is no CN-membership check, since browsers, not
// cluster peers, are the callers. The certificate is re-read fresh on
// every handshake so renewal takes effect without a listener restart.
func UIServerTLS(dataDir string) (*tls.Config, error) {
	certSrc := diskCertSource(dataDir)
	if _, err := certSrc(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return certSrc() },
	}, nil
}

// diskCertSource re-reads this node's TLS identity from dataDir on
// every call, so a cert renewed in place (control's renewal loop) is
// picked up by the very next handshake, with no listener restart.
func diskCertSource(dataDir string) ca.CertSource {
	return func() (*tls.Certificate, error) {
		certPEM, err := os.ReadFile(filepath.Join(dataDir, NodeCertFile))
		if err != nil {
			return nil, errors.New(errors.KindNotFound, "control.TLS", "node cert missing: "+err.Error())
		}
		keyPEM, err := os.ReadFile(filepath.Join(dataDir, NodeKeyFile))
		if err != nil {
			return nil, errors.New(errors.KindNotFound, "control.TLS", "node key missing: "+err.Error())
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, errors.New(errors.KindInternal, "control.TLS", "bad node keypair: "+err.Error())
		}
		return &pair, nil
	}
}

// storeBundleSource re-reads the live CA trust bundle (LoadCATrustBundle)
// on every call, so a rotation's bundle change — or its later retirement
// of the outgoing CA — takes effect on the next handshake. Falls back to
// a single-CA bundle built from fallback on any read error (a transient
// Raft-forwarding hiccup, say): a stale-but-valid trust view keeps
// existing handshakes working rather than failing them outright.
//
// Uses a STALE (local-FSM) read, exactly like membershipCheck's existing
// CN check: a handshake must never block on a linearizable round trip
// just to look up trust, and — critically for InternalClientTLS, whose
// config becomes the forwarder's OWN dial credentials — a linearizable
// read on a non-leader node forwards through that same forwarder,
// which needs this very handshake to complete first. A stale read
// breaks that cycle; it self-heals as raft log replication (which is
// independent of read-forwarding) catches the node's local FSM up.
func storeBundleSource(ctx context.Context, st store.Store, fallback *ca.CA) ca.BundleSource {
	ctx = store.WithStale(ctx)
	return func() (*ca.Bundle, error) {
		b, err := LoadCATrustBundle(ctx, st, fallback)
		if err != nil {
			if fallback != nil {
				if fb, ferr := ca.NewBundle(fallback.Cert); ferr == nil {
					return fb, nil
				}
			}
			return nil, err
		}
		return b, nil
	}
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
// ServeInternalEndpoint runs the node↔node gRPC service on the fixed
// API port (config.PortAPI) until ctx is done.
func ServeInternalEndpoint(ctx context.Context, st *raftstore.Store, clusterCA *ca.CA, dataDir string) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", config.PortAPI))
	if err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "control.ServeInternalEndpoint", "bind :7443: "+err.Error())
	}
	return ServeInternalEndpointOn(ctx, st, clusterCA, dataDir, ln)
}

// ServeInternalEndpointOn is ServeInternalEndpoint over a
// caller-provided listener (tests bind a free loopback port).
func ServeInternalEndpointOn(ctx context.Context, st *raftstore.Store, clusterCA *ca.CA, dataDir string, ln net.Listener) error {
	tlsCfg, err := InternalTLS(ctx, st, clusterCA, dataDir)
	if err != nil {
		return err
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
