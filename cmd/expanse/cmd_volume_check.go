package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	pb "github.com/expanse/expanse/proto"
)

// checkInSync refuses a volume that cannot be verified or resynced safely: both
// need at least two replicas, all of them in sync.
func checkInSync(v *volEntry) error {
	if st := v.st.GetState(); st != pb.VolumeState_VOLUME_STATE_HEALTHY && st != pb.VolumeState_VOLUME_STATE_UNDER_REPLICATED {
		return fmt.Errorf("volume %q is %s; every replica must be in sync first", nameOf(v), stateStr(v.st.GetState()))
	}
	if len(v.st.GetPlacement()) < 2 {
		return fmt.Errorf("volume %q has one replica, so there is nothing to compare it with", nameOf(v))
	}
	for _, p := range v.st.GetPlacement() {
		if !p.GetHealthy() {
			return fmt.Errorf("volume %q: the replica on %s is not healthy; every replica must be in sync first", nameOf(v), p.GetNodeId())
		}
	}
	return nil
}

// checkResync additionally refuses a replica that is not held, or is the primary,
// which DRBD will not rebuild in place.
func checkResync(v *volEntry, node string) error {
	if err := checkInSync(v); err != nil {
		return err
	}
	if node == "" {
		return fmt.Errorf("--node <node> is required: it names the replica to rebuild")
	}
	for _, p := range v.st.GetPlacement() {
		switch {
		case p.GetNodeId() != node:
		case p.GetRole() == pb.ReplicaRole_REPLICA_ROLE_PRIMARY:
			return fmt.Errorf("%s is the primary of volume %q; move-primary first", node, nameOf(v))
		default:
			return nil
		}
	}
	return fmt.Errorf("%s holds no replica of volume %q", node, nameOf(v))
}

func newVolumeCheckCmds(opts *ctlOpts) []*cobra.Command {
	verify := &cobra.Command{
		Use:   "verify <name>",
		Short: "Compare every replica with the others, block by block, while the volume stays in use",
		Long: "The primary starts a DRBD online verify against every peer. Blocks that differ are counted as\n" +
			"out of sync on the primary (`drbdadm status <volume-id>`); nothing is repaired. Use `resync` on the\n" +
			"replica that is wrong. Do not reconnect the volume to repair it: DRBD then forgets the difference.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if err := checkInSync(v); err != nil {
					return err
				}
				if err := putOp(ctx, cl, "verify", v.id, struct{}{}); err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "verify of volume %q requested; it runs on the primary (%s)\n", args[0], v.st.GetPrimary())
				return nil
			})
		},
	}

	var node string
	resync := &cobra.Command{
		Use:   "resync <name> --node <node>",
		Short: "Throw one replica's data away and copy it again from its peers",
		Long: "Use it on the replica a verify found to differ. The replica's own data is DISCARDED and the whole\n" +
			"volume is copied to it again, so the volume runs with fewer good replicas until that finishes.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if err := checkResync(v, node); err != nil {
					return err
				}
				// the node is part of the key so each replica consumes only its own request
				if err := putOp(ctx, cl, "resync", v.id+"/"+node, struct{}{}); err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "volume %q: the replica on %s will discard its data and be rebuilt from its peers\n", args[0], node)
				return nil
			})
		},
	}
	resync.Flags().StringVar(&node, "node", "", "node whose replica is rebuilt")

	return []*cobra.Command{verify, resync}
}
