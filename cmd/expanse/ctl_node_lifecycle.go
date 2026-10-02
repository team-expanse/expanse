package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	pb "github.com/expanse/expanse/proto"
)

// Cluster node lifecycle (§4.8) through the running agent, from any node:
// the agent forwards follower writes to the leader.
func newCtlNodeLifecycleCmds(opts *ctlOpts) []*cobra.Command {
	list := &cobra.Command{
		Use:   "list",
		Short: "List cluster nodes with lifecycle state",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				res, err := c.ListNodes(ctx, &pb.ListNodesRequest{})
				if err != nil {
					return err
				}
				return emit(opts, func() { _ = printNodeList(cmd.OutOrStdout(), res.GetNodes()) }, res)
			})
		},
	}

	cordonCmd := func(use, short string, cordon bool) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <node-id>",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
					_, err := c.SetNodeCordon(ctx, &pb.SetNodeCordonRequest{NodeId: args[0], Cordoned: cordon})
					if err == nil {
						fmt.Fprintf(cmd.OutOrStdout(), "%sed %s\n", use, args[0])
					}
					return err
				})
			},
		}
	}

	drain := &cobra.Command{
		Use:   "drain <node-id>",
		Short: "Cordon a node and verify its resources can be re-placed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ignore, _ := cmd.Flags().GetBool("ignore-unplaceable")
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				res, err := c.DrainNode(ctx, &pb.DrainNodeRequest{NodeId: args[0], IgnoreUnplaceable: ignore})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "drained %s: %d resource(s) to re-place\n", args[0], res.GetResources())
				return nil
			})
		},
	}
	drain.Flags().Bool("ignore-unplaceable", false, "drain even if some resources have nowhere else to run")

	remove := &cobra.Command{
		Use:   "remove <node-id>",
		Short: "Drain, remove from raft, and revoke a node's identity (run on the leader)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			reason, _ := cmd.Flags().GetString("reason")
			confirm, _ := cmd.Flags().GetString("confirm")
			id := args[0]
			// Interlock (§4.8): quorum-breaking removals require --force AND typing the node name.
			if force && confirm == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "removing %s can break the cluster. Type the node name to confirm: ", id)
				var line string
				if _, err := fmt.Fscanln(cmd.InOrStdin(), &line); err != nil {
					return fmt.Errorf("confirmation required")
				}
				confirm = strings.TrimSpace(line)
			}
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				_, err := c.RemoveNode(ctx, &pb.RemoveNodeRequest{NodeId: id, Force: force, Confirm: confirm, Reason: reason})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "removed %s; its identity is revoked (re-join refused)\n", id)
				return nil
			})
		},
	}
	remove.Flags().Bool("force", false, "permit a quorum-breaking removal (still requires typing the node name)")
	remove.Flags().String("confirm", "", "typed confirmation (non-interactive equivalent of the prompt)")
	remove.Flags().String("reason", "", "reason, recorded in the revocation")

	return []*cobra.Command{
		list, cordonCmd("cordon", "Cordon a node (no new placements)", true),
		cordonCmd("uncordon", "Uncordon a node", false), drain, remove,
	}
}

func printNodeList(w io.Writer, nodes []*pb.ClusterNode) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tROLE\tLIFECYCLE\tCORDONED\tRAFT\tAPI\tLAST-SEEN")
	for _, n := range nodes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\t%s\t%s\n", n.GetId(), orDash(n.GetRole()), n.GetLifecycle(),
			n.GetCordoned(), n.GetRaftAddr(), orDash(n.GetApiAddr()),
			time.Unix(0, n.GetLastSeenUnixNs()).UTC().Format(time.RFC3339))
	}
	return tw.Flush()
}
