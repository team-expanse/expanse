package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	pb "github.com/expanse/expanse/proto"
)

// checkRetire refuses a node the volume has no replica on. Whether the node is really
// down is the controller's call, made against the failure monitor rather than this
// command's stale copy of the store.
func checkRetire(v *volEntry, node string) error {
	if node == "" {
		return fmt.Errorf("--node <node> is required: it names the node whose replica is lost")
	}
	for _, p := range v.st.GetPlacement() {
		if p.GetNodeId() == node {
			return nil
		}
	}
	return fmt.Errorf("%s holds no replica of volume %q", node, nameOf(v))
}

func newVolumeLostCmds(opts *ctlOpts) []*cobra.Command {
	var node string
	retire := &cobra.Command{
		Use:   "retire <name> --node <node>",
		Short: "Give up a dead node's replica of a volume now, without waiting for it to return",
		Long: "For a node that is down and not coming back. Its replica is dropped from the volume and its DRBD\n" +
			"identity is forgotten on the survivors; a spare node then takes over if there is one. The controller\n" +
			"refuses a node that is alive, and a volume with no other reachable replica. If the node returns, it\n" +
			"removes its copy.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if err := checkRetire(v, node); err != nil {
					return err
				}
				if err := putOp(ctx, cl, "retire", v.id, map[string]string{"target": args[0], "node": node}); err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "volume %q: the replica on %s will be given up if that node is down\n", args[0], node)
				return nil
			})
		},
	}
	retire.Flags().StringVar(&node, "node", "", "node whose replica is lost")
	return []*cobra.Command{retire}
}
