package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
)

const (
	raftPortDefault = 7444
	joinPortDefault = 7446
)

// EnrollOptions configures `expanse cluster join` (§4.5 protocol, joiner
// side). The joiner generates its key locally, sends only the CSR.
type EnrollOptions struct {
	DataDir string // where to persist enrollment (default /persist/expanse)
	NodeID  string // default: hostname
	Address string // leader join endpoint HOST:7446 (or --discover in T12)
	Token   string
	// BindAddr for this node's raft transport; defaults to
	// 0.0.0.0:<PortRaft>. The leader records the ADVERTISED raft addr
	// — pass AdvertiseAddr (default: derived from bind) so the cluster
	// can dial us.
	BindAddr      string
	AdvertiseAddr string
	Role          string // "voter" (default) | "nonvoter" | "witness"
	APIAddr       string // advertised gRPC API addr (host:7443)
}

// EnrollResult is what a successful join produced.
type EnrollResult struct {
	NodeID    string
	RaftAddr  string
	ClusterID string
	Secret    []byte
	Store     *raftstore.Store // left running (raft must stay up post-AddVoter)
	Cert      *x509.Certificate
	CACert    *x509.Certificate
	Peers     []*pb.JoinPeer
}

// Enroll joins an existing cluster: open an unbootstrapped raft
// transport (so AddVoter has something to replicate to), generate the
// key + CSR locally, call the leader's JoinService, persist certs /
// cluster-id / secret, and return with the raft node still running.
// joinTimeout bounds the whole enrollment RPC flow (dial + Join +
// leader redirect). Without it a hung join service (or a redirect
// loop) hangs the CLI forever; enrollment is a user-facing operation
// and must terminate.
const joinTimeout = 30 * time.Second

func Enroll(ctx context.Context, opts EnrollOptions) (*EnrollResult, error) {
	ctx, cancel := context.WithTimeout(ctx, joinTimeout)
	defer cancel()
	if opts.Address == "" {
		return nil, errors.New(errors.KindInvalid, "control.Enroll", "--address HOST:7446 is required (or --discover once mDNS lands)")
	}
	if opts.Token == "" {
		return nil, errors.New(errors.KindInvalid, "control.Enroll", "--token is required")
	}
	if opts.DataDir == "" {
		opts.DataDir = "/persist/expanse"
	}
	switch opts.Role {
	case "", "voter":
		opts.Role = "voter"
	case "witness", "nonvoter":
	default:
		return nil, errors.New(errors.KindInvalid, "control.Enroll", "--role must be voter, witness or nonvoter (§4.9)")
	}
	if IsClusterNode(opts.DataDir) {
		return nil, errors.New(errors.KindConflict, "control.Enroll", "already a cluster node ("+opts.DataDir+"/"+ClusterIDFile+" exists); re-join after wiping enrollment state")
	}
	nodeID := opts.NodeID
	if nodeID == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			return nil, errors.New(errors.KindInternal, "control.Enroll", "no node id (identity missing and hostname failed)")
		}
		nodeID = h
	}
	bind := opts.BindAddr
	if bind == "" {
		bind = fmt.Sprintf("0.0.0.0:%d", raftPortDefault)
	}
	adv := opts.AdvertiseAddr
	if adv == "" {
		adv = fmt.Sprintf("%s:%d", localIP(), raftPortDefault)
	}

	// The raft transport must be listening before the leader's
	// AddVoter replicates to us.
	st, err := raftstore.Open(raftstore.Config{
		NodeID: nodeID, BindAddr: bind, AdvertiseAddr: adv, DataDir: opts.DataDir + "/" + RaftDir,
	})
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "control.Enroll", "open raft: "+err.Error())
	}

	// Key stays local; only the CSR travels.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		_ = st.Close()
		return nil, errors.New(errors.KindInternal, "control.Enroll", "entropy: "+err.Error())
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: nodeID, Organization: []string{"Expanse"}},
		SignatureAlgorithm: x509.PureEd25519,
	}, priv)
	if err != nil {
		_ = st.Close()
		return nil, errors.Wrap(err, errors.KindInternal, "control.Enroll", "create CSR: "+err.Error())
	}

	resp, err := (&join.Client{}).Join(ctx, opts.Address, &pb.JoinRequest{
		NodeId: nodeID, Csr: csrDER, Token: opts.Token,
		AdvertiseAddr: adv, ApiAddr: opts.APIAddr, Role: opts.Role,
	})
	if err != nil {
		_ = st.Close()
		return nil, err // gRPC status already carries kind + redirect hint
	}

	caCert, err := ca.UnmarshalCert(resp.GetCaCert())
	if err != nil {
		_ = st.Close()
		return nil, errors.New(errors.KindInternal, "control.Enroll", "bad CA cert in response: "+err.Error())
	}
	cert, err := ca.UnmarshalCert(resp.GetNodeCert())
	if err != nil {
		_ = st.Close()
		return nil, errors.New(errors.KindInternal, "control.Enroll", "bad node cert in response: "+err.Error())
	}
	if certPub, ok := cert.PublicKey.(ed25519.PublicKey); !ok || !certPub.Equal(pub) {
		_ = st.Close()
		return nil, errors.New(errors.KindInternal, "control.Enroll", "signed cert does not match our key (leader did not sign the CSR)")
	}

	keyPEM, err := ca.KeyPEM(priv)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := saveClusterID(opts.DataDir, resp.GetClusterId()); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := SaveNodeID(opts.DataDir, nodeID); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := saveSecret(opts.DataDir, resp.GetClusterSecret()); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := saveCAFiles(opts.DataDir, resp.GetCaCert(), caCert); err != nil {
		_ = st.Close()
		return nil, err
	}
	// The sealed CA key lets this node serve joins when it becomes
	// leader (§4.4 layout; sealed to the cluster secret it already has).
	if len(resp.GetSealedCaKey()) > 0 {
		if err := writeFileSync(filepath.Join(opts.DataDir, CAKeyFile), resp.GetSealedCaKey(), 0o600); err != nil {
			_ = st.Close()
			return nil, errors.New(errors.KindInternal, "control.Enroll", "save sealed CA key: "+err.Error())
		}
	}
	if err := saveNodeTLS(opts.DataDir, resp.GetNodeCert(), keyPEM); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := SaveRaftAddr(opts.DataDir, bind, adv); err != nil {
		_ = st.Close()
		return nil, err
	}

	return &EnrollResult{
		NodeID: nodeID, RaftAddr: adv, ClusterID: resp.GetClusterId(),
		Secret: resp.GetClusterSecret(), Store: st, Cert: cert, CACert: caCert, Peers: resp.GetPeers(),
	}, nil
}

// saveCAFiles persists a PEM CA (as returned in JoinResponse).
func saveCAFiles(dataDir string, caPEM []byte, caCert *x509.Certificate) error {
	dir := filepath.Join(dataDir, CADir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dataDir, CAFile), caPEM, 0o644)
}

// Leave removes a node from the cluster: raft.RemoveServer + delete
// /nodes/<id>. Guardrails: refuse to remove the leader (transfer first
// — T14 builds the full lifecycle) and refuse to shrink below quorum.
func Leave(ctx context.Context, st *raftstore.Store, nodeID string) error {
	if st.IsLeader() {
		if nodeID == localID(st) {
			return errors.New(errors.KindConflict, "control.Leave", "refusing to remove the leader; transfer leadership first (`expanse ctl raft transfer-leadership`)")
		}
	}
	nodes, err := st.List(ctx, store.Key(join.NodesKeyPrefix))
	if err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "control.Leave", "list nodes: "+err.Error())
	}
	if len(nodes) <= 2 {
		return errors.New(errors.KindConflict, "control.Leave", "refusing to shrink below 2 nodes (no quorum redundancy)")
	}
	if err := st.RemoveServer(nodeID); err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "control.Leave", "RemoveServer: "+err.Error())
	}
	entry, err := st.Get(ctx, store.Key(join.NodesKeyPrefix+nodeID))
	if err == nil {
		if err := st.Delete(ctx, store.Key(join.NodesKeyPrefix+nodeID), entry.Revision); err != nil {
			return errors.Wrap(err, errors.KindInternal, "control.Leave", "delete node record: "+err.Error())
		}
	}
	return nil
}

// storeLocalID is filled by the raftstore adapter (NodeID is not
// exported through the generic store interface, so Leave compares
// against the cfg-captured id passed by the CLI instead).
func localID(st *raftstore.Store) string { return st.NodeID() }
