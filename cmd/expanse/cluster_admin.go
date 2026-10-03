package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/api"
	"github.com/expanse/expanse/internal/cluster/control"
	pb "github.com/expanse/expanse/proto"
)

// clusterAdminOpts reach the running agent's socket, or, when no agent
// answers, the node's data dir to open the cluster store from.
type clusterAdminOpts struct {
	ctlOpts
	dataDir, nodeID string
}

func addClusterAdminFlags(cmd *cobra.Command) *clusterAdminOpts {
	o := &clusterAdminOpts{ctlOpts: ctlOpts{output: "table", socket: defaultSocket, timeout: 60 * time.Second}}
	f := cmd.Flags()
	f.StringVar(&o.socket, "socket", defaultSocket, "agent unix socket (used when the agent is running)")
	f.StringVar(&o.dataDir, "data-dir", "/persist/expanse", "persistent state directory (used when the agent is stopped)")
	f.StringVar(&o.nodeID, "node-id", "", "node ID (default: the one recorded at init/join)")
	return o
}

// withClusterClient runs fn against the local agent or, when none is
// running, against the same API served in-process over the local store.
func withClusterClient(cmd *cobra.Command, o *clusterAdminOpts, fn func(context.Context, pb.NodeServiceClient) error) error {
	if agentAnswers(o.socket) {
		return withClient(cmd, &o.ctlOpts, fn)
	}
	sock, stop, err := serveOfflineAPI(cmd.Context(), o.dataDir, o.nodeID)
	if err != nil {
		return err
	}
	defer stop()
	inner := o.ctlOpts
	inner.socket = sock
	return withClient(cmd, &inner, fn)
}

// serveOfflineAPI opens the cluster store and serves the agent API on a
// private socket until stop is called.
func serveOfflineAPI(ctx context.Context, dataDir, nodeID string) (string, func(), error) {
	clusterID, secret, _, err := control.LoadCluster(dataDir)
	if err != nil {
		return "", nil, fmt.Errorf("no agent running and no cluster enrollment in %s: %w", dataDir, err)
	}
	st, closeStore, err := openClusterStore(dataDir, nodeID)
	if err != nil {
		return "", nil, err
	}
	if !control.WaitForLeader(ctx, st, 30*time.Second) {
		closeStore()
		return "", nil, fmt.Errorf("no leader reachable within 30s (quorum unavailable?)")
	}
	dir, err := os.MkdirTemp("", "expanse-cli-")
	if err != nil {
		closeStore()
		return "", nil, err
	}
	srv := api.NewServer(nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.Cluster = &api.ClusterIdentity{ID: clusterID, Secret: secret}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	sock := filepath.Join(dir, "agent.sock")
	go func() { _ = srv.Serve(sctx, sock); close(done) }()
	stop := func() { cancel(); <-done; closeStore(); _ = os.RemoveAll(dir) }
	for i := 0; i < 250 && !agentAnswers(sock); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	return sock, stop, nil
}

func newClusterLeaveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "leave [node-id]",
		Short: "Remove a node from the cluster (default: this node); same as `ctl node remove`",
		Args:  cobra.MaximumNArgs(1),
	}
	o := addClusterAdminFlags(cmd)
	reason := cmd.Flags().String("reason", "left the cluster", "reason, recorded in the revocation")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		target := localNodeID(o)
		if len(args) == 1 {
			target = args[0]
		}
		return withClusterClient(cmd, o, func(ctx context.Context, c pb.NodeServiceClient) error {
			if _, err := c.RemoveNode(ctx, &pb.RemoveNodeRequest{NodeId: target, Reason: *reason}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s; its identity is revoked (re-join refused)\n", target)
			return nil
		})
	}
	return cmd
}

// localNodeID is --node-id, else the ID recorded at init/join, else the hostname.
func localNodeID(o *clusterAdminOpts) string {
	if o.nodeID != "" {
		return o.nodeID
	}
	if id := control.LoadNodeID(o.dataDir); id != "" {
		return id
	}
	h, _ := os.Hostname()
	return h
}

func newClusterTokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage join tokens"}

	create := &cobra.Command{Use: "create", Short: "Create a join token"}
	o := addClusterAdminFlags(create)
	ttl := create.Flags().String("ttl", "15m", "token time-to-live")
	uses := create.Flags().Int("uses", 1, "max number of joins with this token")
	forNode := create.Flags().String("for-node", "", "scope this token to recovering exactly this already-enrolled node_id (required to re-join/recover an existing node; a plain token can only enroll a new one)")
	create.RunE = func(cmd *cobra.Command, args []string) error {
		d, err := parseDuration(*ttl)
		if err != nil {
			return fmt.Errorf("--ttl: %w", err)
		}
		return withClusterClient(cmd, o, func(ctx context.Context, c pb.NodeServiceClient) error {
			res, err := c.CreateJoinToken(ctx, &pb.CreateJoinTokenRequest{
				TtlSeconds: int64(d / time.Second), Uses: int32(*uses), ForNode: *forNode,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.GetToken())
			return nil
		})
	}

	list := &cobra.Command{Use: "list", Short: "List join tokens"}
	lo := addClusterAdminFlags(list)
	list.RunE = func(cmd *cobra.Command, args []string) error {
		return withClusterClient(cmd, lo, func(ctx context.Context, c pb.NodeServiceClient) error {
			res, err := c.ListJoinTokens(ctx, &pb.ListJoinTokensRequest{})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NONCE\tEXPIRES\tUSES\tMAX\tBY")
			for _, t := range res.GetTokens() {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", t.GetNonce(),
					time.Unix(0, t.GetExpiresUnixNs()).UTC().Format(time.RFC3339), t.GetUses(), t.GetMax(), t.GetBy())
			}
			return tw.Flush()
		})
	}

	revoke := &cobra.Command{Use: "revoke <token|nonce>", Short: "Revoke a join token", Args: cobra.ExactArgs(1)}
	ro := addClusterAdminFlags(revoke)
	revoke.RunE = func(cmd *cobra.Command, args []string) error {
		return withClusterClient(cmd, ro, func(ctx context.Context, c pb.NodeServiceClient) error {
			_, err := c.RevokeJoinToken(ctx, &pb.RevokeJoinTokenRequest{TokenOrNonce: args[0]})
			return err
		})
	}

	cmd.AddCommand(create, list, revoke)
	return cmd
}

// newClusterCACmd groups CA rotation (Phase 10 X2); run from any node.
func newClusterCACmd() *cobra.Command {
	cmd := &cobra.Command{Use: "ca", Short: "Cluster CA rotation (Phase 10 X2)"}

	rotate := &cobra.Command{Use: "rotate", Short: "Generate a fresh CA and start rotation (old CA stays trusted until `ca complete`)"}
	o := addClusterAdminFlags(rotate)
	rotate.RunE = func(cmd *cobra.Command, args []string) error {
		return withClusterClient(cmd, o, func(ctx context.Context, c pb.NodeServiceClient) error {
			if _, err := c.RotateCA(ctx, &pb.RotateCARequest{}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "CA rotation started: the new CA is now primary; the old CA stays trusted. "+
				"Every node reissues its own cert onto the new CA the next time its renewal loop ticks "+
				"(no restart needed). Check progress with `cluster ca status`; once every node has "+
				"caught up, run `cluster ca complete` to retire the old CA.")
			return nil
		})
	}

	status := &cobra.Command{Use: "status", Short: "Show CA rotation progress"}
	so := addClusterAdminFlags(status)
	status.RunE = func(cmd *cobra.Command, args []string) error {
		return withClusterClient(cmd, so, func(ctx context.Context, c pb.NodeServiceClient) error {
			res, err := c.GetCARotation(ctx, &pb.GetCARotationRequest{})
			if err != nil {
				return err
			}
			printCARotation(cmd.OutOrStdout(), res)
			return nil
		})
	}

	complete := &cobra.Command{Use: "complete", Short: "Retire the outgoing CA once every node has renewed onto the new one"}
	co := addClusterAdminFlags(complete)
	complete.RunE = func(cmd *cobra.Command, args []string) error {
		return withClusterClient(cmd, co, func(ctx context.Context, c pb.NodeServiceClient) error {
			if _, err := c.CompleteCARotation(ctx, &pb.CompleteCARotationRequest{}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "CA rotation complete: outgoing CA retired.")
			return nil
		})
	}

	cmd.AddCommand(rotate, status, complete)
	return cmd
}

func printCARotation(w io.Writer, r *pb.GetCARotationResponse) {
	switch {
	case !r.GetRotating():
		fmt.Fprintln(w, "no rotation in progress")
	case len(r.GetPending()) == 0:
		fmt.Fprintf(w, "rotating: primary CA fingerprint %s\n", r.GetFingerprint())
		fmt.Fprintln(w, "every node has renewed onto the new CA; safe to run `cluster ca complete`")
	default:
		fmt.Fprintf(w, "rotating: primary CA fingerprint %s\n", r.GetFingerprint())
		fmt.Fprintf(w, "pending nodes (not yet renewed): %s\n", strings.Join(r.GetPending(), ", "))
	}
}
