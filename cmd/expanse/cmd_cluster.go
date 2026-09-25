package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/discovery"
	"github.com/expanse/expanse/internal/config"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
)

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Cluster bootstrap, enrollment and status (Phase 03)",
	}
	cmd.AddCommand(newClusterInitCmd())
	cmd.AddCommand(newClusterJoinCmd())
	cmd.AddCommand(newClusterRestoreCmd())
	cmd.AddCommand(newClusterStatusCmd())
	cmd.AddCommand(newClusterLeaveCmd())
	cmd.AddCommand(newClusterTokenCmd())
	cmd.AddCommand(newClusterDiscoverCmd())
	cmd.AddCommand(newClusterCACmd())
	return cmd
}

func newClusterInitCmd() *cobra.Command {
	var (
		dataDir   string
		name      string
		nodeID    string
		advertise string
		expect    int
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Bootstrap a new cluster (self as first voter)",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := control.Init(cmd.Context(), control.InitOptions{
				DataDir: dataDir, NodeID: nodeID, Name: name,
				AdvertiseAddr: advertise, Expect: expect,
			})
			if err != nil {
				return err
			}
			defer res.Store.Close() //nolint:errcheck — CLI exit
			fmt.Printf("cluster initialized: %s (name %q)\n", res.ClusterID, name)
			fmt.Printf("data dir:    %s\n", dataDir)
			fmt.Printf("raft addr:   %s\n", advertise)
			fmt.Printf("\nJoin command (single-use, 15m):\n  %s\n", res.JoinCommand)
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
	cmd.Flags().StringVar(&name, "name", "expanse", "cluster name")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "node ID (default: hostname)")
	cmd.Flags().StringVar(&advertise, "advertise-addr", defaultAdvertise(), "raft advertise address (IP:7444)")
	cmd.Flags().IntVar(&expect, "expect", 3, "expected number of cluster nodes")
	return cmd
}

func defaultAdvertise() string {
	return fmt.Sprintf("%s:%d", control.LocalIP(), config.PortRaft)
}

// newClusterRestoreCmd implements `expanse cluster restore` (Phase 8 X6):
// the "plus one command" rebuild side of destroy-and-restore. It shells
// out to restic, which already owns "backup credentials" as the
// RESTIC_REPOSITORY/RESTIC_PASSWORD (and backend-specific, e.g. AWS_*)
// environment variables an operator already holds for whatever backed
// this node up — no separate credential plumbing needed here. Run
// before starting the daemon; --data-dir then holds cluster identity,
// raft state and configuration exactly as they were at backup time, so
// starting the daemon afterward rejoins under the original identity
// with no separate re-init step, even with no live quorum to catch up
// from (the harder scenario X5 deferred to this stream).
func newClusterRestoreCmd() *cobra.Command {
	var dataDir string
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore this node's persistent state from a restic backup (Phase 8 X6)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterRestore(cmd, dataDir)
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory to restore")
	return cmd
}

func runClusterRestore(cmd *cobra.Command, dataDir string) error {
	if _, err := exec.LookPath("restic"); err != nil {
		return fmt.Errorf("restic not found on PATH: %w", err)
	}
	restic := exec.CommandContext(cmd.Context(), "restic", "restore", "latest", "--target", "/", "--include", dataDir)
	restic.Stdout = cmd.OutOrStdout()
	restic.Stderr = cmd.ErrOrStderr()
	if err := restic.Run(); err != nil {
		return fmt.Errorf("restic restore: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "restored %s from the latest backup\n", dataDir)
	return nil
}

func newClusterJoinCmd() *cobra.Command {
	var (
		dataDir   string
		nodeID    string
		address   string
		token     string
		discover  bool
		apiAddr   string
		role      string
		bind      string
		advertise string
	)
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Join an existing cluster using a join token",
		RunE: func(cmd *cobra.Command, args []string) error {
			joinAddr := address
			if discover {
				found, err := discoverJoinAddr(cmd.Context())
				if err != nil {
					return err
				}
				joinAddr = found
				fmt.Printf("discovered join endpoint: %s\n", joinAddr)
			}
			res, err := control.Enroll(cmd.Context(), control.EnrollOptions{
				DataDir: dataDir, NodeID: nodeID, Address: joinAddr, Token: token,
				APIAddr: apiAddr, Role: role, BindAddr: bind, AdvertiseAddr: advertise,
			})
			if err != nil {
				return err
			}
			defer res.Store.Close() //nolint:errcheck — CLI exit
			fmt.Printf("joined cluster %s as %s (%s)\n", res.ClusterID, res.NodeID, res.RaftAddr)
			fmt.Printf("start the daemon: expansionsd (or: expanse agent)\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "node ID (default: hostname)")
	cmd.Flags().StringVar(&address, "address", "", "leader join endpoint HOST:7446")
	cmd.Flags().StringVar(&token, "token", "", "join token (expanse-join-…)")
	cmd.Flags().BoolVar(&discover, "discover", false, "discover the join endpoint via mDNS instead of --address")
	cmd.Flags().StringVar(&apiAddr, "api-addr", fmt.Sprintf(":%d", config.PortAPI), "advertised API address")
	cmd.Flags().StringVar(&role, "role", "voter", "node role (§4.9): voter | witness (full raft voter, zero capacity, skips storage/net-mesh)")
	cmd.Flags().StringVar(&bind, "bind-addr", "", "raft transport bind addr (default 0.0.0.0:7444)")
	cmd.Flags().StringVar(&advertise, "advertise-addr", "", "raft transport advertised addr (default: derived from bind)")
	return cmd
}

// discoverJoinAddr browses mDNS for cluster members and returns the
// first join endpoint (IP:7446). Discovery only finds candidates; the
// token still authorizes (§4.6).
func discoverJoinAddr(ctx context.Context) (string, error) {
	records, err := discovery.Browse(ctx, 3*time.Second)
	if err != nil {
		return "", err
	}
	for _, r := range records {
		if r.ClusterID != "" && r.Role != discovery.RoleUnjoined && r.Addr != "" {
			return net.JoinHostPort(r.Addr, strconv.Itoa(config.PortJoin)), nil
		}
	}
	return "", fmt.Errorf("no cluster found on the local network (mDNS requires same L2; use --address for routed networks)")
}

// newClusterDiscoverCmd implements `expanse cluster discover` — lists
// found clusters and unjoined nodes on the local network.
func newClusterDiscoverCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "List clusters and unjoined nodes found via mDNS",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout+2*time.Second)
			defer cancel()
			records, err := discovery.Browse(ctx, timeout)
			if err != nil {
				return err
			}
			clusters := map[string][]discovery.Record{}
			var unjoined []discovery.Record
			for _, r := range records {
				if r.ClusterID == "" || r.Role == discovery.RoleUnjoined {
					unjoined = append(unjoined, r)
				} else {
					clusters[r.ClusterID] = append(clusters[r.ClusterID], r)
				}
			}
			if len(clusters) == 0 && len(unjoined) == 0 {
				fmt.Println("nothing found (mDNS requires the same L2 network)")
				return nil
			}
			for id, nodes := range clusters {
				fmt.Printf("cluster %s — %d node(s) visible:\n", id, len(nodes))
				for _, r := range nodes {
					fmt.Printf("  %-20s %-8s %s api=%d\n", r.NodeID, r.Role, r.Addr, r.APIPort)
				}
			}
			if len(unjoined) > 0 {
				fmt.Printf("unjoined nodes — %d:\n", len(unjoined))
				for _, r := range unjoined {
					fmt.Printf("  %-20s %s api=%d\n", r.NodeID, r.Addr, r.APIPort)
				}
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "mDNS browse duration")
	return cmd
}

// openClusterStore opens the local raft store for CLI operations. The
// node ID defaults to the hostname (matching init/join). It fails with
// a hint when a running daemon holds the Bolt lock.
func openClusterStore(dataDir, nodeID string) (*raftstore.Store, func(), error) {
	if !control.IsClusterNode(dataDir) {
		return nil, nil, fmt.Errorf("%s is not a cluster node (no cluster-id); run `expanse cluster init` or `join`", dataDir)
	}
	if nodeID == "" {
		nodeID = control.LoadNodeID(dataDir)
		if nodeID == "" {
			return nil, nil, fmt.Errorf("no node-id recorded in %s (pre-Phase03 enrollment?); pass --node-id", dataDir)
		}
	}
	// Rebind on THIS node's recorded raft address (persisted at init/
	// join) so multi-node clusters on one host each get their own port.
	bind, adv := control.LoadRaftAddr(dataDir)
	if bind == "" {
		bind = fmt.Sprintf("0.0.0.0:%d", config.PortRaft)
	}
	if adv == "" {
		adv = fmt.Sprintf("%s:%d", control.LocalIP(), config.PortRaft)
	}
	st, err := raftstore.Open(raftstore.Config{
		NodeID:        nodeID,
		BindAddr:      bind,
		AdvertiseAddr: adv,
		DataDir:       filepath.Join(dataDir, control.RaftDir),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open store (is the daemon running? it holds the lock): %w", err)
	}
	// Write/read forwarding to the leader (mirrors agent.go's cluster
	// wiring exactly): without it, any command run from a non-leader
	// node fails outright — a linearizable read/write only works
	// locally on whichever node happens to be leader. CLI commands
	// (token, ca, leave) must work from any enrolled node.
	if _, _, clusterCA, err := control.LoadCluster(dataDir); err == nil {
		fwd := raftstore.NewGRPCForwarder(
			func() string { return st.Leader() },
			func(raftAddr string) (string, bool) {
				host, _, err := net.SplitHostPort(raftAddr)
				if err != nil {
					return "", false
				}
				return net.JoinHostPort(host, fmt.Sprintf("%d", config.PortAPI)), true
			},
		)
		if tlsCfg, err := control.InternalClientTLS(context.Background(), st, clusterCA, dataDir); err == nil {
			fwd.SetDialCreds(credentials.NewTLS(tlsCfg))
			st.SetForwarder(fwd.Forward)
			st.SetReadForwarder(fwd)
		}
	}
	return st, func() { _ = st.Close() }, nil
}

func newClusterStatusCmd() *cobra.Command {
	var (
		dataDir, nodeID string
		socket          string
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show cluster name/ID, quorum, leader, per-node state, generation",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			st, cleanup, err := openClusterStore(dataDir, nodeID)
			if err != nil {
				// The local daemon holds the bolt lock + raft port: ask
				// IT for the report instead (socket RPC).
				return clusterStatusViaSocket(ctx, socket)
			}
			defer cleanup()
			control.WaitForLeader(ctx, st, 10*time.Second)
			rep, err := control.Status(ctx, st)
			if err != nil {
				return err
			}
			fmt.Print(control.Render(rep))
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "node ID (default: hostname)")
	cmd.Flags().StringVar(&socket, "socket", "/run/expanse/agent.sock", "agent unix socket (used when the daemon holds the store)")
	return cmd
}

// clusterStatusViaSocket renders `cluster status` through the local
// agent's GetClusterStatus RPC (the daemon owns the bolt lock and the
// raft port while running).
func clusterStatusViaSocket(ctx context.Context, socket string) error {
	if _, err := os.Stat(socket); err != nil {
		return fmt.Errorf("store busy and agent socket %s unavailable (is expansed running?): %w", socket, err)
	}
	conn, err := grpc.NewClient("unix://"+filepath.ToSlash(socket),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	res, err := pb.NewNodeServiceClient(conn).GetClusterStatus(ctx, &pb.GetClusterStatusRequest{})
	if err != nil {
		return fmt.Errorf("GetClusterStatus: %w", err)
	}
	var rep control.Report
	if err := json.Unmarshal(res.GetReportJson(), &rep); err != nil {
		return fmt.Errorf("decode report: %w", err)
	}
	fmt.Print(control.Render(&rep))
	return nil
}

func newClusterLeaveCmd() *cobra.Command {
	var dataDir, nodeID string
	cmd := &cobra.Command{
		Use:   "leave [node-id]",
		Short: "Remove a node from the cluster (default: this node)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			local, _ := os.Hostname()
			target := nodeID
			if len(args) == 1 {
				target = args[0]
			}
			if target == "" {
				target = local
			}
			st, cleanup, err := openClusterStore(dataDir, local)
			if err != nil {
				return err
			}
			defer cleanup()
			return control.Leave(cmd.Context(), st, target)
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "node ID of the local node")
	return cmd
}

// newClusterCACmd groups CA rotation commands (Phase 10 X2). Like
// `token create`, these reopen the local raft store directly
// (openClusterStore) rather than going through the running daemon, so
// — exactly as documented for `token create` — the invoking node's own
// daemon must not be holding the raft port when the command runs; the
// other nodes of the cluster are unaffected and stay up throughout.
func newClusterCACmd() *cobra.Command {
	cmd := &cobra.Command{Use: "ca", Short: "Cluster CA rotation (Phase 10 X2)"}
	cmd.AddCommand(newClusterCARotateCmd(), newClusterCAStatusCmd(), newClusterCACompleteCmd())
	return cmd
}

func newClusterCARotateCmd() *cobra.Command {
	var dataDir, nodeID string
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Generate a fresh CA and start rotation (old CA stays trusted until `ca complete`)",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, cleanup, err := openClusterStore(dataDir, nodeID)
			if err != nil {
				return err
			}
			defer cleanup()
			_, secret, _, err := control.LoadCluster(dataDir)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
			defer cancel()
			// Reopening the raft store just now means this node is a
			// rejoining follower with no idea yet who's leader; give it
			// a real chance to hear from one before the actual op,
			// rather than a short-lived CAS failing outright (mirrors
			// `cluster status`'s own WaitForLeader use).
			if !control.WaitForLeader(ctx, st, 30*time.Second) {
				return fmt.Errorf("no leader reachable within 30s")
			}
			if _, err := control.RotateCA(ctx, st, secret, time.Now()); err != nil {
				return err
			}
			fmt.Println("CA rotation started: the new CA is now primary; the old CA stays trusted. " +
				"Every node reissues its own cert onto the new CA the next time its renewal loop ticks " +
				"(no restart needed). Check progress with `cluster ca status`; once every node has " +
				"caught up, run `cluster ca complete` to retire the old CA.")
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "node ID (default: hostname)")
	return cmd
}

func newClusterCAStatusCmd() *cobra.Command {
	var dataDir, nodeID string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show CA rotation progress",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, cleanup, err := openClusterStore(dataDir, nodeID)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
			defer cancel()
			if !control.WaitForLeader(ctx, st, 30*time.Second) {
				return fmt.Errorf("no leader reachable within 30s")
			}
			status, err := control.CARotationStatus(ctx, st)
			if err != nil {
				return err
			}
			if !status.Rotating {
				fmt.Println("no rotation in progress")
				return nil
			}
			fmt.Printf("rotating: primary CA fingerprint %s\n", status.Fingerprint)
			if len(status.Pending) == 0 {
				fmt.Println("every node has renewed onto the new CA; safe to run `cluster ca complete`")
			} else {
				fmt.Printf("pending nodes (not yet renewed): %v\n", status.Pending)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "node ID (default: hostname)")
	return cmd
}

func newClusterCACompleteCmd() *cobra.Command {
	var dataDir, nodeID string
	cmd := &cobra.Command{
		Use:   "complete",
		Short: "Retire the outgoing CA once every node has renewed onto the new one",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, cleanup, err := openClusterStore(dataDir, nodeID)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
			defer cancel()
			if !control.WaitForLeader(ctx, st, 30*time.Second) {
				return fmt.Errorf("no leader reachable within 30s")
			}
			if err := control.CompleteCARotation(ctx, st); err != nil {
				return err
			}
			fmt.Println("CA rotation complete: outgoing CA retired.")
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "node ID (default: hostname)")
	return cmd
}

func newClusterTokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage join tokens"}
	cmd.AddCommand(newClusterTokenCreateCmd(), newClusterTokenListCmd(), newClusterTokenRevokeCmd())
	return cmd
}

func newClusterTokenCreateCmd() *cobra.Command {
	var dataDir, nodeID, ttl, forNode string
	var uses int
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a join token",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, cleanup, err := openClusterStore(dataDir, nodeID)
			if err != nil {
				return err
			}
			defer cleanup()
			d, err := parseDuration(ttl)
			if err != nil {
				return fmt.Errorf("--ttl: %w", err)
			}
			_, secret, _, err := control.LoadCluster(dataDir)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			if !control.WaitForLeader(ctx, st, 10*time.Second) {
				return fmt.Errorf("no leader after 10s (quorum unavailable?)")
			}
			tok, err := control.CreateToken(ctx, st, clusterIDOf(dataDir), secret, d, uses, nodeID, forNode)
			if err != nil {
				return err
			}
			fmt.Println(tok)
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "node ID (default: hostname)")
	cmd.Flags().StringVar(&ttl, "ttl", "15m", "token time-to-live")
	cmd.Flags().IntVar(&uses, "uses", 1, "max number of joins with this token")
	cmd.Flags().StringVar(&forNode, "for-node", "", "scope this token to recovering exactly this already-enrolled node_id (required to re-join/recover an existing node; a plain token can only enroll a new one)")
	return cmd
}

func newClusterTokenListCmd() *cobra.Command {
	var dataDir, nodeID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List join tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, cleanup, err := openClusterStore(dataDir, nodeID)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			control.WaitForLeader(ctx, st, 10*time.Second)
			toks, err := control.ListTokens(ctx, st)
			if err != nil {
				return err
			}
			fmt.Println("NONCE\tEXPIRES\tUSES\tMAX\tBY")
			for _, t := range toks {
				fmt.Printf("%s\t%s\t%d\t%d\t%s\n", t.Nonce, t.Expires.Format(time.RFC3339), t.Uses, t.Max, t.By)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "node ID (default: hostname)")
	return cmd
}

func newClusterTokenRevokeCmd() *cobra.Command {
	var dataDir string
	cmd := &cobra.Command{
		Use:   "revoke <token|nonce>",
		Short: "Revoke a join token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, cleanup, err := openClusterStore(dataDir, "")
			if err != nil {
				return err
			}
			defer cleanup()
			_, secret, _, err := control.LoadCluster(dataDir)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			if !control.WaitForLeader(ctx, st, 10*time.Second) {
				return fmt.Errorf("no leader after 10s (quorum unavailable?)")
			}
			return control.RevokeToken(ctx, st, secret, args[0])
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
	return cmd
}

func clusterIDOf(dataDir string) string {
	id, err := os.ReadFile(filepath.Join(dataDir, control.ClusterIDFile))
	if err != nil {
		return ""
	}
	return string(trimLocal(id))
}

func trimLocal(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
