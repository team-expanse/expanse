package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/discovery"
	"github.com/expanse/expanse/internal/config"
	"github.com/expanse/expanse/internal/store/raftstore"
)

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Cluster bootstrap, enrollment and status (Phase 03)",
	}
	cmd.AddCommand(newClusterInitCmd())
	cmd.AddCommand(newClusterJoinCmd())
	cmd.AddCommand(newClusterStatusCmd())
	cmd.AddCommand(newClusterLeaveCmd())
	cmd.AddCommand(newClusterTokenCmd())
	cmd.AddCommand(newClusterDiscoverCmd())
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

func newClusterJoinCmd() *cobra.Command {
	var (
		dataDir  string
		nodeID   string
		address  string
		token    string
		discover bool
		apiAddr  string
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
				DataDir: dataDir, NodeID: nodeID, Address: joinAddr, Token: token, APIAddr: apiAddr,
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
	st, err := raftstore.Open(raftstore.Config{
		NodeID:        nodeID,
		BindAddr:      fmt.Sprintf("0.0.0.0:%d", config.PortRaft),
		AdvertiseAddr: fmt.Sprintf("%s:%d", control.LocalIP(), config.PortRaft),
		DataDir:       filepath.Join(dataDir, control.RaftDir),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open store (is the daemon running? it holds the lock): %w", err)
	}
	return st, func() { _ = st.Close() }, nil
}

func newClusterStatusCmd() *cobra.Command {
	var dataDir, nodeID string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show cluster name/ID, quorum, leader, per-node state, generation",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, cleanup, err := openClusterStore(dataDir, nodeID)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
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
	return cmd
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

func newClusterTokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage join tokens"}
	cmd.AddCommand(newClusterTokenCreateCmd(), newClusterTokenListCmd(), newClusterTokenRevokeCmd())
	return cmd
}

func newClusterTokenCreateCmd() *cobra.Command {
	var dataDir, nodeID, ttl string
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
			tok, err := control.CreateToken(ctx, st, clusterIDOf(dataDir), secret, d, uses, nodeID)
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
