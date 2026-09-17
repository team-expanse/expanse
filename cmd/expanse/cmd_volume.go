package main

import (
	"context"
	"fmt"
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
		Short: "Exvol volume management",
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
				fmt.Printf("volume %q create requested — poll `expanse ctl volume list`\n", args[0])
				return nil
			})
		},
	}
	create.Flags().StringVar(&sizeStr, "size", "", "volume size (e.g. 10Gi)")
	create.Flags().IntVar(&repl, "replication", 3, "replication factor (1..5)")
	create.Flags().StringVar(&class, "class", "default", "storage class")

	list := &cobra.Command{
		Use:   "list",
		Short: "List volumes with state, primary, and placement",
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				res, err := cl.ListKeyValue(ctx, &pb.ListKeyValueRequest{Prefix: "/volumes/"})
				if err != nil {
					return err
				}
				rows := map[string]*volumeRow{}
				var order []string
				for _, e := range res.GetEntries() {
					id, suffix, ok := splitVolumeKey(e.GetKey())
					if !ok {
						continue
					}
					r := rows[id]
					if r == nil {
						r = &volumeRow{id: id}
						rows[id] = r
						order = append(order, id)
					}
					if suffix == "spec" {
						var s pb.VolumeSpec
						if pbproto.Unmarshal(e.GetValue(), &s) == nil {
							r.name = s.GetName()
							r.size = s.GetSizeBytes()
							r.repl = s.GetReplication()
						}
					} else if suffix == "status" {
						var st pb.VolumeStatus
						if pbproto.Unmarshal(e.GetValue(), &st) == nil {
							r.state = st.GetState().String()
							r.primary = st.GetPrimary()
							for _, p := range st.GetPlacement() {
								r.nodes = append(r.nodes, p.GetNodeId())
							}
						}
					}
				}
				if len(order) == 0 {
					fmt.Println("no volumes")
					return nil
				}
				fmt.Printf("%-24s %-14s %-12s %-10s %-3s %s\n", "ID", "NAME", "SIZE", "STATE", "R", "REPLICAS (PRIMARY)")
				for _, id := range order {
					r := rows[id]
					fmt.Printf("%-24s %-14s %-12d %-10s %-3d %s (%s)\n",
						id, r.name, r.size, r.state, r.repl, strings.Join(r.nodes, ","), r.primary)
				}
				return nil
			})
		},
	}

	cmd.AddCommand(create, list)
	return cmd
}

type volumeRow struct {
	id      string
	name    string
	size    uint64
	repl    int32
	state   string
	primary string
	nodes   []string
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
