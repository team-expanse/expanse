package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"
	pbproto "google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/quantity"
	pb "github.com/expanse/expanse/proto"
)

// volEntry is one volume's store view for the ctl commands (T15 §4.8).
type volEntry struct {
	id   string
	spec *pb.VolumeSpec
	st   *pb.VolumeStatus
}

// loadVolumes indexes all volumes by name from the local store copy (Stale), so
// inspect and list still answer when the control plane has no leader.
func loadVolumes(ctx context.Context, cl pb.NodeServiceClient) (map[string]*volEntry, error) {
	res, err := cl.ListKeyValue(ctx, &pb.ListKeyValueRequest{Prefix: "/volumes/", Stale: true})
	if err != nil {
		return nil, err
	}
	out := map[string]*volEntry{}
	for _, e := range res.GetEntries() {
		id, suffix, ok := splitVolumeKey(e.GetKey())
		if !ok {
			continue
		}
		v := out[id]
		if v == nil {
			v = &volEntry{id: id}
			out[id] = v
		}
		switch suffix {
		case "spec":
			var s pb.VolumeSpec
			if pbproto.Unmarshal(e.GetValue(), &s) == nil {
				v.spec = &s
			}
		case "status":
			var s pb.VolumeStatus
			if pbproto.Unmarshal(e.GetValue(), &s) == nil {
				v.st = &s
			}
		}
	}
	// Key by name; fall back to the ID for half-written volumes.
	byName := map[string]*volEntry{}
	for _, v := range out {
		key := v.id
		if v.spec != nil && v.spec.GetName() != "" {
			key = v.spec.GetName()
		}
		byName[key] = v
	}
	return byName, nil
}

// resolveVol finds one volume by name (or ID).
func resolveVol(ctx context.Context, cl pb.NodeServiceClient, name string) (*volEntry, error) {
	vols, err := loadVolumes(ctx, cl)
	if err != nil {
		return nil, err
	}
	v, ok := vols[name]
	if !ok {
		return nil, fmt.Errorf("no such volume %q", name)
	}
	return v, nil
}

// putOp writes an operation request for the leader's controller /
// runtimes to consume (the ctl never touches the raft store directly).
func putOp(ctx context.Context, cl pb.NodeServiceClient, kind, volID string, val any) error {
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	_, err = cl.PutKeyValue(ctx, &pb.PutKeyValueRequest{
		Key:   "/volumes/_ops/" + kind + "/" + volID,
		Value: raw,
	})
	return err
}

func newVolumeOpsCmds(opts *ctlOpts) []*cobra.Command {
	var toNode, sizeStr string

	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a volume (removes every replica's logical volume)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if err := putOp(ctx, cl, "delete", v.id, map[string]string{"target": args[0]}); err != nil {
					return err
				}
				fmt.Printf("volume %q delete requested\n", args[0])
				return nil
			})
		},
	}

	resize := &cobra.Command{
		Use:   "resize <name>",
		Short: "Grow the volume (grow-only; a shrink is refused because DRBD cannot shrink)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			b, err := quantity.ParseBytes(sizeStr)
			if err != nil {
				return fmt.Errorf("--size: %w", err)
			}
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if have := v.spec.GetSizeBytes(); uint64(b.N) <= have {
					return fmt.Errorf("--size %s is not larger than the current %s; volumes only grow", sizeStr, humanBytes(have))
				}
				if err := putOp(ctx, cl, "resize", v.id, map[string]any{"target": args[0], "sizeBytes": b.N}); err != nil {
					return err
				}
				fmt.Printf("volume %q resize to %s requested\n", args[0], sizeStr)
				return nil
			})
		},
	}
	resize.Flags().StringVar(&sizeStr, "size", "", "new size (e.g. 20Gi)")

	insp := &cobra.Command{
		Use:   "inspect <name>",
		Short: "Placement, primary and per-replica role and health",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if v.st == nil {
					return fmt.Errorf("volume %q has no status yet", args[0])
				}
				printInspect(c.OutOrStdout(), v)
				return nil
			})
		},
	}

	move := &cobra.Command{
		Use:   "move-primary <name>",
		Short: "Move the primary to a node that already holds a replica",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if toNode == "" {
					return fmt.Errorf("--to <node> is required")
				}
				if err := putOp(ctx, cl, "move-primary", v.id, map[string]string{"target": args[0], "to": toNode}); err != nil {
					return err
				}
				fmt.Printf("primary move of %q to %s requested\n", args[0], toNode)
				return nil
			})
		},
	}

	diverged := &cobra.Command{
		Use:   "diverged",
		Short: "List volumes that need manual recovery (split-brain is never resolved automatically)",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return divergedList(c, opts) },
	}
	move.Flags().StringVar(&toNode, "to", "", "destination node (must hold a replica)")

	return []*cobra.Command{del, resize, insp, move, diverged}
}

// printInspect renders `volume inspect`: which replicas exist and how each reports.
func printInspect(w io.Writer, v *volEntry) {
	fmt.Fprintf(w, "volume %s (%s)\n", nameOf(v), v.id)
	fmt.Fprintf(w, "  state:    %s\n", stateStr(v.st.GetState()))
	fmt.Fprintf(w, "  primary:  %s\n", v.st.GetPrimary())
	fmt.Fprintf(w, "  size:     %s\n", humanBytes(v.spec.GetSizeBytes()))
	fmt.Fprintf(w, "  %-16s %-12s %-8s %s\n", "REPLICA", "ROLE", "HEALTHY", "LAST SEEN")
	for _, p := range v.st.GetPlacement() {
		seen := "never"
		if p.GetLastSeenUnixNano() > 0 {
			seen = relTime(p.GetLastSeenUnixNano())
		}
		fmt.Fprintf(w, "  %-16s %-12s %-8t %s\n", p.GetNodeId(), roleStr(p.GetRole()), p.GetHealthy(), seen)
	}
}

// divergedList is the listing form of `volume diverged`.
func divergedList(c *cobra.Command, opts *ctlOpts) error {
	return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
		w := c.OutOrStdout()
		vols, err := loadVolumes(ctx, cl)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(vols))
		for n, v := range vols {
			if v.st != nil && v.st.GetState() == pb.VolumeState_VOLUME_STATE_NEEDS_MANUAL_RECOVERY {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			fmt.Fprintln(w, "no diverged volumes")
			return nil
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(w, "volume %s — NeedsManualRecovery\n", n)
		}
		return nil
	})
}

// stateStr renders a VolumeState exactly as internal/storage/model.go's
// VolumeState string constants spell it — an explicit table, not a
// per-underscore title-caser: VOLUME_STATE_READONLY is one compound
// word with no separating underscore, so naive title-casing produced
// "Readonly" instead of "ReadOnly" (silently unnoticed until this
// state became reachable — vol-degraded.nix, G6.12).
func stateStr(vs pb.VolumeState) string {
	switch vs {
	case pb.VolumeState_VOLUME_STATE_CREATING:
		return "Creating"
	case pb.VolumeState_VOLUME_STATE_HEALTHY:
		return "Healthy"
	case pb.VolumeState_VOLUME_STATE_DEGRADED:
		return "Degraded"
	case pb.VolumeState_VOLUME_STATE_READONLY:
		return "ReadOnly"
	case pb.VolumeState_VOLUME_STATE_RESYNCING:
		return "Resyncing"
	case pb.VolumeState_VOLUME_STATE_FAILED:
		return "Failed"
	case pb.VolumeState_VOLUME_STATE_DELETING:
		return "Deleting"
	case pb.VolumeState_VOLUME_STATE_NEEDS_MANUAL_RECOVERY:
		return "NeedsManualRecovery"
	default:
		return "Unspecified"
	}
}

func nameOf(v *volEntry) string {
	if v.spec != nil && v.spec.GetName() != "" {
		return v.spec.GetName()
	}
	return v.id
}

func roleStr(r pb.ReplicaRole) string {
	switch r {
	case pb.ReplicaRole_REPLICA_ROLE_PRIMARY:
		return "primary"
	case pb.ReplicaRole_REPLICA_ROLE_SECONDARY:
		return "secondary"
	case pb.ReplicaRole_REPLICA_ROLE_RESYNCING:
		return "resyncing"
	case pb.ReplicaRole_REPLICA_ROLE_STALE:
		return "stale"
	}
	return "?"
}

// humanBytes renders a byte count in the usual units.
func humanBytes(n uint64) string {
	b := quantity.Bytes{N: int64(n)}
	return b.String()
}

// relTime formats a UnixNano timestamp as a coarse age.
func relTime(un int64) string {
	d := time.Since(time.Unix(0, un))
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Hour).String()
	}
}
