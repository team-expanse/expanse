package control

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/config"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store/raftstore"
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
