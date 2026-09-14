package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/blocks/apply"
	"github.com/expanse/expanse/internal/blocks/logs"
	"github.com/expanse/expanse/internal/blocks/service"
	"github.com/expanse/expanse/internal/blocks/validate"
	pb "github.com/expanse/expanse/proto"
)

// newBlockCmd builds `expanse ctl block ...` and `expanse ctl catalog ...`
// (PHASE04.md §7). All commands ride the agent unix socket's gRPC API.
func newBlockCmd(opts *ctlOpts) (*cobra.Command, *cobra.Command) {
	block := &cobra.Command{Use: "block", Short: "Block lifecycle, logs, and events (§7)"}

	nsFlag := func(c *cobra.Command) *string {
		var ns string
		c.Flags().StringVarP(&ns, "namespace", "n", "default", "namespace")
		return &ns
	}
	outputFlag := func(c *cobra.Command, def string) *string {
		var o string
		c.Flags().StringVarP(&o, "output", "o", def, "output format (table|json|yaml)")
		return &o
	}

	var out string
	list := &cobra.Command{
		Use:   "list",
		Short: "List blocks (all namespaces with -n \"\")",
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = out
			return withBlockClient(cmd, opts, func(ctx context.Context, c pb.BlockServiceClient) error {
				resp, err := c.List(ctx, &pb.ListBlocksRequest{Namespace: *nsFlag(cmd)})
				if err != nil {
					return fmt.Errorf("List: %w", err)
				}
				return emit(opts, func() {
					w := cmd.OutOrStdout()
					fmt.Fprintf(w, "%-24s %-20s %-9s %-8s\n", "NAME", "TYPE", "REPLICAS", "PHASE")
					for _, b := range resp.GetBlocks() {
						m := b.GetMetadata()
						fmt.Fprintf(w, "%-24s %-20s %-9d %-8s\n",
							m.GetNamespace()+"/"+m.GetName(),
							b.GetSpec().GetType(),
							b.GetSpec().GetReplicas(),
							b.GetStatus().GetPhase().String())
					}
				}, resp.GetBlocks())
			})
		},
	}
	_ = outputFlag(list, "table")

	get := &cobra.Command{
		Use:   "get <name>",
		Short: "Get one block",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := nsFlag(cmd)
			return withBlockClient(cmd, opts, func(ctx context.Context, c pb.BlockServiceClient) error {
				b, err := c.Get(ctx, &pb.GetBlockRequest{Namespace: *ns, Name: args[0]})
				if err != nil {
					return fmt.Errorf("Get: %w", err)
				}
				return emit(opts, nil, b)
			})
		},
	}

	applyCmd := &cobra.Command{
		Use:   "apply -f <file>",
		Short: "Apply a block.yaml (create or update)",
		RunE: func(cmd *cobra.Command, args []string) error {
			file, _ := cmd.Flags().GetString("file")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			var data []byte
			var err error
			if file == "-" {
				data, err = io.ReadAll(os.Stdin)
			} else {
				data, err = os.ReadFile(file)
			}
			if err != nil {
				return err
			}
			b, err := apply.Parse(bytes.NewReader(data))
			if err != nil {
				return err
			}
			if dryRun {
				// --dry-run: admission validation only, locally — by
				// construction this touches neither the store nor the API
				// (zero writes guaranteed). Structural rules run with a
				// zero Context; type/schema rules (V3/V19/V20) need the
				// server-side catalog and are checked on real apply.
				if ves := validate.Validate(b, validate.Context{}); len(ves) > 0 {
					return fmt.Errorf("dry-run rejected: %v", ves[0])
				}
				fmt.Fprintf(cmd.OutOrStdout(), "dry-run: %s/%s accepted (no changes written)\n",
					b.GetMetadata().GetNamespace(), b.GetMetadata().GetName())
				return nil
			}
			return withBlockClient(cmd, opts, func(ctx context.Context, c pb.BlockServiceClient) error {
				// Idiomatic apply: try Update; create on NotFound.
				if _, err := c.Update(ctx, b); err == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "updated %s/%s\n",
						b.GetMetadata().GetNamespace(), b.GetMetadata().GetName())
					return nil
				}
				if _, err := c.Create(ctx, b); err != nil {
					return fmt.Errorf("Create: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "created %s/%s\n",
					b.GetMetadata().GetNamespace(), b.GetMetadata().GetName())
				return nil
			})
		},
	}
	applyCmd.Flags().StringP("file", "f", "", "block.yaml file (or - for stdin)")
	applyCmd.Flags().Bool("dry-run", false, "validate only; no store writes")
	_ = applyCmd.MarkFlagRequired("file")

	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a block",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := nsFlag(cmd)
			wait, _ := cmd.Flags().GetBool("wait")
			return withBlockClient(cmd, opts, func(ctx context.Context, c pb.BlockServiceClient) error {
				if _, err := c.Delete(ctx, &pb.DeleteBlockRequest{Namespace: *ns, Name: args[0]}); err != nil {
					return fmt.Errorf("Delete: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "deleted %s/%s\n", *ns, args[0])
				if wait {
					// Phase 05: wait for teardown completion; today the
					// delete removes desired state immediately.
					fmt.Fprintln(cmd.OutOrStdout(), "(--wait: teardown tracking arrives in Phase 05)")
				}
				return nil
			})
		},
	}
	del.Flags().Bool("wait", false, "wait for teardown to finish")

	scale := &cobra.Command{
		Use:   "scale <name> --replicas N",
		Short: "Change replica count",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := nsFlag(cmd)
			n, _ := cmd.Flags().GetInt32("replicas")
			return withBlockClient(cmd, opts, func(ctx context.Context, c pb.BlockServiceClient) error {
				b, err := c.Scale(ctx, &pb.ScaleRequest{Namespace: *ns, Name: args[0], Replicas: n})
				if err != nil {
					return fmt.Errorf("Scale: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "scaled %s/%s to %d replicas\n",
					b.GetMetadata().GetNamespace(), b.GetMetadata().GetName(), n)
				return nil
			})
		},
	}
	scale.Flags().Int32("replicas", 1, "target replica count")
	_ = scale.MarkFlagRequired("replicas")

	restart := &cobra.Command{
		Use:   "restart <name> [--replica N]",
		Short: "Restart a replica (or all with --replica -1)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := nsFlag(cmd)
			replica, _ := cmd.Flags().GetInt32("replica")
			return withBlockClient(cmd, opts, func(ctx context.Context, c pb.BlockServiceClient) error {
				b, err := c.Restart(ctx, &pb.RestartRequest{Namespace: *ns, Name: args[0], Replica: replica})
				if err != nil {
					return fmt.Errorf("Restart: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "restart requested for %s/%s (replica %d)\n",
					b.GetMetadata().GetNamespace(), b.GetMetadata().GetName(), replica)
				return nil
			})
		},
	}
	restart.Flags().Int32("replica", -1, "replica index (-1 = all)")

	events := &cobra.Command{
		Use:   "events <name>",
		Short: "Stream lifecycle events for one block",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := nsFlag(cmd)
			return withBlockClient(cmd, opts, func(ctx context.Context, c pb.BlockServiceClient) error {
				stream, err := c.Watch(ctx, &pb.WatchBlocksRequest{Namespace: *ns, Name: args[0]})
				if err != nil {
					return fmt.Errorf("Watch: %w", err)
				}
				w := cmd.OutOrStdout()
				for {
					ev, err := stream.Recv()
					if err != nil {
						return nil // stream closed (context deadline/cancel)
					}
					if ev.GetBlock() != nil {
						fmt.Fprintf(w, "%s  %-8s  phase=%s\n",
							time.Now().UTC().Format(time.RFC3339),
							ev.GetType(), ev.GetBlock().GetStatus().GetPhase())
					}
				}
			})
		},
	}

	logsCmd := &cobra.Command{
		Use:   "logs <namespace/name>",
		Short: "Stream block logs from journald (SYSLOG_IDENTIFIER=expanse-block-<ns>-<name>-<idx>)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlBlockLogs(cmd, opts, args[0]) },
	}
	var logReplica int32
	var logAllReplicas, logFollow bool
	var logTail int
	var logSince string
	logsCmd.Flags().Int32Var(&logReplica, "replica", 0, "replica index")
	logsCmd.Flags().BoolVar(&logAllReplicas, "all-replicas", false, "interleave all replicas (per-line replica prefix)")
	logsCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "follow the log stream")
	logsCmd.Flags().IntVar(&logTail, "tail", 0, "number of past lines to start from")
	logsCmd.Flags().StringVar(&logSince, "since", "", "start point: duration (1h) or RFC3339 timestamp")

	explain := &cobra.Command{
		Use:   "explain <name>",
		Short: "Why is this block pending? Per-node filter/score breakdown (§7)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := nsFlag(cmd)
			return withBlockClient(cmd, opts, func(ctx context.Context, c pb.BlockServiceClient) error {
				resp, err := c.Explain(ctx, &pb.ExplainRequest{Namespace: *ns, Name: args[0]})
				if err != nil {
					return fmt.Errorf("Explain: %w", err)
				}
				fmt.Fprint(cmd.OutOrStdout(), service.RenderExplain(resp))
				return nil
			})
		},
	}

	block.AddCommand(list, get, applyCmd, del, scale, restart, events, explain, logsCmd)

	catalog := &cobra.Command{Use: "catalog", Short: "Block type catalog (§6)"}
	catalog.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List available block types",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCatalogClient(cmd, opts, func(ctx context.Context, c pb.CatalogServiceClient) error {
				resp, err := c.ListTypes(ctx, &pb.ListTypesRequest{})
				if err != nil {
					return fmt.Errorf("ListTypes: %w", err)
				}
				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "%-24s %-10s %s\n", "TYPE", "VERSION", "DESCRIPTION")
				for _, t := range resp.GetTypes() {
					fmt.Fprintf(w, "%-24s %-10s %s\n", t.GetName(), t.GetVersion(), t.GetDescription())
				}
				return nil
			})
		},
	})
	catalog.AddCommand(&cobra.Command{
		Use:   "show <category/name>",
		Short: "Show one block type (including its JSON Schema)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCatalogClient(cmd, opts, func(ctx context.Context, c pb.CatalogServiceClient) error {
				t, err := c.GetType(ctx, &pb.GetTypeRequest{Name: args[0]})
				if err != nil {
					return fmt.Errorf("GetType: %w", err)
				}
				return emit(opts, nil, t)
			})
		},
	})

	return block, catalog
}

// withBlockClient dials the agent socket and runs fn with a BlockService.
func withBlockClient(cmd *cobra.Command, opts *ctlOpts, fn func(ctx context.Context, c pb.BlockServiceClient) error) error {
	conn, err := dial(opts)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()
	return fn(ctx, pb.NewBlockServiceClient(conn))
}

func withCatalogClient(cmd *cobra.Command, opts *ctlOpts, fn func(ctx context.Context, c pb.CatalogServiceClient) error) error {
	conn, err := dial(opts)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()
	return fn(ctx, pb.NewCatalogServiceClient(conn))
}

func logReplicaOf(cmd *cobra.Command) int32 {
	v, _ := cmd.Flags().GetInt32("replica")
	return v
}

func flagBool(cmd *cobra.Command, name string) bool { v, _ := cmd.Flags().GetBool(name); return v }
func flagInt(cmd *cobra.Command, name string) int   { v, _ := cmd.Flags().GetInt(name); return v }
func flagString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

func ctlBlockLogs(cmd *cobra.Command, opts *ctlOpts, ref string) error {
	ns, name := "default", ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		ns, name = ref[:i], ref[i+1:]
	}
	q, err := logs.ParseFlags(logs.Flags{
		Replica:     logReplicaOf(cmd),
		AllReplicas: flagBool(cmd, "all-replicas"),
		Follow:      flagBool(cmd, "follow"),
		Tail:        flagInt(cmd, "tail"),
		Since:       flagString(cmd, "since"),
	})
	if err != nil {
		return err
	}
	q.Namespace, q.Name = ns, name
	conn, err := dial(opts)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()
	client := pb.NewBlockServiceClient(conn)

	// Single replica, or --all-replicas: sequential streams per index
	// (interleave order = replica index) with a per-line prefix.
	w := cmd.OutOrStdout()
	lastIdx := int32(0)
	if q.AllReplicas {
		lastIdx = 63 // sanity cap; we stop at the first unhosted replica
	}
	for idx := q.Replica; ; idx++ {
		if q.AllReplicas && idx > lastIdx {
			break
		}
		stream, serr := client.StreamLogs(ctx, &pb.LogsRequest{
			Namespace: q.Namespace, Name: q.Name, Replica: idx,
			Follow: q.Follow, Tail: int32(q.Tail), Since: q.Since,
		})
		if serr != nil {
			if q.AllReplicas && idx > q.Replica {
				return nil // walked past the last hosted replica
			}
			return fmt.Errorf("StreamLogs: %w", serr)
		}
		prefix := ""
		if q.AllReplicas {
			prefix = fmt.Sprintf("[%d] ", idx)
		}
		for {
			line, rerr := stream.Recv()
			if rerr != nil {
				break // this replica's stream ended; next replica
			}
			fmt.Fprintf(w, "%s%s\n", prefix, line.GetLine())
		}
		if !q.AllReplicas {
			break // single replica: one stream, done
		}
	}
	return nil
}
