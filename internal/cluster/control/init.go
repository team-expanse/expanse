package control

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/generation"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	"github.com/google/uuid"
)

// InitOptions configures `expanse cluster init` (§4.5 steps 1–8).
type InitOptions struct {
	DataDir       string // default /persist/expanse
	NodeID        string // default: hostname
	Name          string // cluster name, default "expanse"
	AdvertiseAddr string // raft advertise addr host:port, e.g. "10.0.0.1:7444"
	BindAddr      string // raft bind addr, defaults to AdvertiseAddr or 0.0.0.0:7444
	Expect        int    // expected cluster size (informational)
	Version       string // stamped into meta, default DefaultVersion
	JoinHost      string // host advertised in the printed join command, default advertise host
	JoinPort      int    // join endpoint port, default config.PortJoin (7446)
}

// InitResult carries what init produced.
type InitResult struct {
	ClusterID   string
	Secret      []byte
	Token       string // fresh single-use join token
	JoinCommand string // ready-to-print join command
	Store       *raftstore.Store
	CA          *ca.CA
}

// ClusterMeta is the /cluster/meta value.
type ClusterMeta struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Created int64  `json:"created"` // unix-nano
	Version string `json:"version"`
	Expect  int    `json:"expect"`
}

// Init bootstraps a new cluster: assert fresh data dir, generate
// cluster ID/secret/CA, start a single-voter raft, write /cluster/meta,
// /nodes/<id> and generation 1, mint a join token, and print the join
// command (§4.5).
func Init(ctx context.Context, opts InitOptions) (*InitResult, error) {
	if opts.DataDir == "" {
		opts.DataDir = "/persist/expanse"
	}
	if opts.Version == "" {
		opts.Version = DefaultVersion
	}
	// 1. Assert not already in a cluster.
	if IsClusterNode(opts.DataDir) {
		return nil, errors.New(errors.KindConflict, "control.Init", "already a cluster node ("+opts.DataDir+"/"+ClusterIDFile+" exists); use `expanse cluster status`")
	}
	nodeID := opts.NodeID
	if nodeID == "" {
		h, err := os.Hostname()
		if err != nil {
			return nil, err
		}
		nodeID = h
	}

	// 2. Cluster ID + 32-byte secret.
	clusterID := uuid.NewString()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, errors.New(errors.KindInternal, "control.Init", "entropy: "+err.Error())
	}

	// 3. CA.
	clusterCA, err := ca.Generate(clusterID, time.Now())
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "control.Init", "generate CA: "+err.Error())
	}

	// 4. Bootstrap Raft with self as the only voter.
	bind := opts.BindAddr
	if bind == "" {
		bind = fmt.Sprintf("0.0.0.0:%d", raftPortDefault)
	}
	adv := opts.AdvertiseAddr
	if adv == "" {
		adv = fmt.Sprintf("%s:%d", localIP(), raftPortDefault)
	}
	st, err := raftstore.Open(raftstore.Config{
		NodeID: nodeID, BindAddr: bind, AdvertiseAddr: adv,
		DataDir: opts.DataDir + "/" + RaftDir, Bootstrap: true,
	})
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "control.Init", "bootstrap raft: "+err.Error())
	}
	waitLeader(ctx, st)

	// 5–7. /cluster/meta, /nodes/<id>, generation 1 (single-op Txn puts).
	// Generation 1 is the empty bootstrap snapshot: meta + empty desired
	// state + pointer, so the generation history starts complete (§4.7).
	meta, _ := json.Marshal(ClusterMeta{ID: clusterID, Name: opts.Name, Created: time.Now().UnixNano(), Version: opts.Version, Expect: opts.Expect})
	rec, _ := json.Marshal(join.NodeRecord{ID: nodeID, RaftAddr: adv, Role: "voter", JoinedAt: time.Now().UnixNano()})
	gen1Meta, _ := json.Marshal(generation.Generation{ //nolint:errcheck — plain struct
		Number:      1,
		CreatedAt:   time.Now().UTC(),
		CreatedBy:   "system",
		Description: "cluster bootstrap",
		Hash:        generation.Hash(map[store.Key][]byte{}),
	})
	if _, err := st.Txn(ctx, []store.Op{
		{Kind: store.OpPut, Key: store.Key(MetaKey), Value: meta},
		{Kind: store.OpPut, Key: store.Key(join.NodesKeyPrefix + nodeID), Value: rec},
		{Kind: store.OpPut, Key: generation.DataKey(1), Value: generation.EncodeSnapshot(map[store.Key][]byte{})},
		{Kind: store.OpPut, Key: generation.MetaKey(1), Value: gen1Meta},
		{Kind: store.OpPut, Key: generation.CurrentKey, Value: []byte(GenerationOne)},
	}); err != nil {
		_ = st.Close()
		return nil, errors.Wrap(err, errors.KindInternal, "control.Init", "write meta: "+err.Error())
	}

	// Persist enrollment state before printing anything.
	if err := saveClusterID(opts.DataDir, clusterID); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := SaveNodeID(opts.DataDir, nodeID); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := saveSecret(opts.DataDir, secret); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := saveCA(opts.DataDir, clusterCA, secret); err != nil {
		_ = st.Close()
		return nil, err
	}
	// The init node needs its own TLS identity too: the join endpoint
	// (:7446) presents it, and Phase 03+ mTLS uses it. Same layout as
	// Enroll persists for joiners (tls/node-cert.pem, tls/node-key.pem).
	nodeCert, nodePriv, err := clusterCA.IssueNode(nodeID, hostnameOrLocal(), nil, time.Now())
	if err != nil {
		_ = st.Close()
		return nil, errors.Wrap(err, errors.KindInternal, "control.Init", "issue node cert: "+err.Error())
	}
	keyPEM, err := ca.KeyPEM(nodePriv)
	if err != nil {
		_ = st.Close()
		return nil, errors.Wrap(err, errors.KindInternal, "control.Init", "encode node key: "+err.Error())
	}
	if err := saveNodeTLS(opts.DataDir, ca.MarshalCert(nodeCert), keyPEM); err != nil {
		_ = st.Close()
		return nil, errors.Wrap(err, errors.KindInternal, "control.Init", "save node TLS: "+err.Error())
	}
	if err := SaveRaftAddr(opts.DataDir, bind, adv); err != nil {
		_ = st.Close()
		return nil, errors.Wrap(err, errors.KindInternal, "control.Init", "save raft addr: "+err.Error())
	}

	// 8. Join command with a fresh token.
	token, _, err := join.CreateToken(ctx, st, clusterID, secret, join.DefaultTokenTTL, 1, nodeID, "")
	if err != nil {
		_ = st.Close()
		return nil, errors.Wrap(err, errors.KindInternal, "control.Init", "mint token: "+err.Error())
	}
	host := opts.JoinHost
	if host == "" {
		host = hostOf(adv)
	}
	port := opts.JoinPort
	if port == 0 {
		port = joinPortDefault
	}
	return &InitResult{
		ClusterID: clusterID, Secret: secret, Token: token, Store: st, CA: clusterCA,
		JoinCommand: fmt.Sprintf("expanse cluster join --address %s:%d --token %s", host, port, token),
	}, nil
}

func hostOf(addr string) string {
	for i := 0; i < len(addr); i++ {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}

// hostnameOrLocal is the SAN hostname for the init node's own cert:
// the machine's hostname, falling back to "localhost".
func hostnameOrLocal() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}

// waitLeader polls until the bootstrapped node wins its election.
func waitLeader(ctx context.Context, st *raftstore.Store) {
	deadline := time.Now().Add(10 * time.Second)
	for !st.IsLeader() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return // Init proceeds; election retries are handled by raft
		}
		time.Sleep(25 * time.Millisecond)
	}
}
