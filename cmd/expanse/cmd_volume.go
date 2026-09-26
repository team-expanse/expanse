package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	pbproto "google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/quantity"
	pb "github.com/expanse/expanse/proto"
)

// volumePendingKeyPrefix must match storage.PendingCreateKey. The ctl
// speaks protobuf-only through the agent socket (the daemon holds the
// raft lock), so the key lives here as a literal.
const volumePendingKeyPrefix = "/volumes/_pending/"

// newVolumeCmd is the minimal `expanse ctl volume` surface (Phase 06
// T10 slice of §4.8; T15 grows the rest). It talks to the local agent
// socket only — never the raft store directly (the daemon holds the
// Bolt lock). `create` writes a pending-creation request; the leader's
// volume runtime performs placement; nodes converge from the store.
func newVolumeCmd(opts *ctlOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "Replicated volume management",
	}

	var sizeStr, class string
	var repl int
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a volume (placement happens on the leader)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			b, err := quantity.ParseBytes(sizeStr)
			if err != nil {
				return fmt.Errorf("--size: %w", err)
			}
			raw, err := pbproto.Marshal(&pb.VolumeSpec{
				Name:        args[0],
				SizeBytes:   uint64(b.N),
				Class:       class,
				Replication: int32(repl),
			})
			if err != nil {
				return err
			}
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				if _, err := cl.PutKeyValue(ctx, &pb.PutKeyValueRequest{
					Key:   volumePendingKeyPrefix + args[0],
					Value: raw,
				}); err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "volume %q create requested — poll `expanse ctl volume list`\n", args[0])
				return nil
			})
		},
	}
	create.Flags().StringVar(&sizeStr, "size", "", "volume size (e.g. 10Gi)")
	create.Flags().IntVar(&repl, "replication", 0, "replication factor (1..5); unset uses the class's, on as many nodes as exist up to it")
	create.Flags().StringVar(&class, "class", "default", "storage class")

	list := &cobra.Command{
		Use:   "list",
		Short: "List volumes with state, primary, and placement, then pending requests",
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				vols, err := loadVolumes(ctx, cl)
				if err != nil {
					return err
				}
				pending, err := loadPending(ctx, cl)
				if err != nil {
					return err
				}
				printVolumeList(c.OutOrStdout(), vols, pending)
				return nil
			})
		},
	}

	cmd.AddCommand(append([]*cobra.Command{create, list}, newVolumeOpsCmds(opts)...)...)
	return cmd
}

// splitVolumeKey breaks /volumes/<id>/spec|status into (id, suffix).
func splitVolumeKey(key string) (id, suffix string, ok bool) {
	const p = "/volumes/"
	if !strings.HasPrefix(key, p) || strings.HasPrefix(key[len(p):], "_pending/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(key, p)
	i := strings.Index(rest, "/")
	if i <= 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// placementReasonPrefix must match storage.PlacementReasonPrefix.
const placementReasonPrefix = "/volume-placement-reasons/"

// loadPending maps each queued create request's name to why it isn't placed yet ("" if unknown).
func loadPending(ctx context.Context, cl pb.NodeServiceClient) (map[string]string, error) {
	reqs, err := cl.ListKeyValue(ctx, &pb.ListKeyValueRequest{Prefix: volumePendingKeyPrefix, Stale: true})
	if err != nil {
		return nil, err
	}
	reasons, err := cl.ListKeyValue(ctx, &pb.ListKeyValueRequest{Prefix: placementReasonPrefix, Stale: true})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range reqs.GetEntries() {
		out[strings.TrimPrefix(e.GetKey(), volumePendingKeyPrefix)] = ""
	}
	for _, e := range reasons.GetEntries() {
		if name := strings.TrimPrefix(e.GetKey(), placementReasonPrefix); out[name] == "" {
			if _, queued := out[name]; queued {
				out[name] = string(e.GetValue())
			}
		}
	}
	return out, nil
}

func printVolumeList(w io.Writer, vols map[string]*volEntry, pending map[string]string) {
	if len(vols) == 0 && len(pending) == 0 {
		fmt.Fprintln(w, "no volumes")
		return
	}
	fmt.Fprintf(w, "%-24s %-14s %-12s %-16s %-24s %s\n", "ID", "NAME", "SIZE", "STATE", "REPLICAS", "NODES (PRIMARY)")
	for _, name := range slices.Sorted(maps.Keys(vols)) {
		v := vols[name]
		var nodes []string
		for _, p := range v.st.GetPlacement() {
			nodes = append(nodes, p.GetNodeId())
		}
		fmt.Fprintf(w, "%-24s %-14s %-12s %-16s %-24s %s (%s)\n", v.id, name, humanBytes(v.spec.GetSizeBytes()),
			stateStr(v.st.GetState()), replicaSummary(v), strings.Join(nodes, ","), v.st.GetPrimary())
	}
	for _, name := range slices.Sorted(maps.Keys(pending)) {
		reason := pending[name]
		if reason == "" {
			reason = "waiting for the leader"
		}
		fmt.Fprintf(w, "%-24s %-14s pending: %s\n", "-", name, reason)
	}
}

// replicaSummary is "members of target", flagging a volume with no redundancy.
func replicaSummary(v *volEntry) string {
	s := fmt.Sprintf("%d of %d", len(v.st.GetPlacement()), v.spec.GetReplication())
	if v.st.GetState() == pb.VolumeState_VOLUME_STATE_UNDER_REPLICATED {
		s += " (no redundancy)"
	}
	return s
}
