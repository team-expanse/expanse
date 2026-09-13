package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/expanse/expanse/proto"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
)

const (
	defaultSocket = "/run/expanse/agent.sock"
)

type ctlOpts struct {
	output  string // table | json | yaml
	socket  string
	timeout time.Duration
}

func newCtlCmd() *cobra.Command {
	opts := &ctlOpts{output: "table", socket: defaultSocket, timeout: 30 * time.Second}
	cmd := &cobra.Command{
		Use:   "ctl",
		Short: "Control the local node agent over its unix socket",
	}
	pf := cmd.PersistentFlags()
	pf.StringVarP(&opts.output, "output", "o", "table", "output format (table|json|yaml; JSON is stable and scriptable, table is for humans)")
	pf.StringVar(&opts.socket, "socket", defaultSocket, "agent unix socket path")
	pf.DurationVar(&opts.timeout, "timeout", 30*time.Second, "per-request timeout")

	node := &cobra.Command{Use: "node", Short: "Node status, inventory, and health"}
	node.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Health summary table",
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlNodeStatus(cmd, opts) },
	})
	node.AddCommand(&cobra.Command{
		Use:   "inspect",
		Short: "Full inventory",
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlNodeInspect(cmd, opts) },
	})
	node.AddCommand(&cobra.Command{
		Use:   "health",
		Short: "Per-check results",
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlNodeHealth(cmd, opts) },
	})
	node.AddCommand(&cobra.Command{
		Use:   "shutdown",
		Short: "Request a graceful agent shutdown",
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlShutdown(cmd, opts) },
	})
	// §4.8 cluster-node lifecycle: these open the node's raft store
	// from --data-dir (daemon must not hold the lock) and their
	// mutations must run on the leader.
	node.AddCommand(newCtlNodeLifecycleCmds()...)
	cmd.AddCommand(node)

	res := &cobra.Command{Use: "resource", Short: "Desired-state resources"}
	res.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List resources",
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlResourceList(cmd, opts) },
	})
	get := &cobra.Command{
		Use:   "get <id>",
		Short: "Get one resource",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlResourceGet(cmd, opts, args[0]) },
	}
	res.AddCommand(get)
	apply := &cobra.Command{
		Use:   "apply -f <file>",
		Short: "Apply a desired-state YAML document",
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlResourceApply(cmd, opts) },
	}
	var file string
	apply.Flags().StringVarP(&file, "file", "f", "", "YAML file (or - for stdin)")
	res.AddCommand(apply)
	del := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a resource (desired state + managed object)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlResourceDelete(cmd, opts, args[0]) },
	}
	res.AddCommand(del)
	cmd.AddCommand(res)

	gen := &cobra.Command{Use: "generation", Short: "Desired-state generation history and rollback"}
	gen.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List generations (oldest first)",
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlGenList(cmd, opts) },
	})
	gen.AddCommand(&cobra.Command{
		Use:   "show <n>",
		Short: "Show one generation's metadata and keys",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlGenShow(cmd, opts, args[0]) },
	})
	gen.AddCommand(&cobra.Command{
		Use:   "diff <a> <b>",
		Short: "Diff two generations' desired state",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlGenDiff(cmd, opts, args[0], args[1]) },
	})
	rb := &cobra.Command{
		Use:   "rollback [<n>]",
		Short: "Restore generation n's state as a NEW generation (default: n-1). History is append-only — roll forward by rolling back again.",
		Args:  cobra.MaximumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlGenRollback(cmd, opts, args) },
	}
	gen.AddCommand(rb)
	cmd.AddCommand(gen)

	rec := &cobra.Command{
		Use:   "reconcile",
		Short: "Trigger a reconcile and stream progress",
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlReconcile(cmd, opts) },
	}
	var dryRun bool
	rec.Flags().BoolVar(&dryRun, "dry-run", false, "plan only, apply nothing")
	cmd.AddCommand(rec)

	ev := &cobra.Command{
		Use:   "events",
		Short: "Stream store events",
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlEvents(cmd, opts) },
	}
	var follow bool
	ev.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming")
	cmd.AddCommand(ev)
	return cmd
}

func dial(opts *ctlOpts) (*grpc.ClientConn, error) {
	if _, err := os.Stat(opts.socket); err != nil {
		return nil, fmt.Errorf("agent socket %s not available (is expansed running?): %w", opts.socket, err)
	}
	return grpc.NewClient("unix://"+filepath.ToSlash(opts.socket),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}

func withClient(cmd *cobra.Command, opts *ctlOpts, fn func(ctx context.Context, c pb.NodeServiceClient) error) error {
	conn, err := dial(opts)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()
	return fn(ctx, pb.NewNodeServiceClient(conn))
}

// emit prints v according to the output format. JSON is stable and
// scriptable; table output is for humans and may change.
func emit(opts *ctlOpts, table func(), v any) error {
	switch opts.output {
	case "json":
		// protojson (lowerCamelCase proto field names) makes the output
		// stable and scriptable; std json is the fallback for non-proto
		// values.
		if m, ok := v.(proto.Message); ok {
			out, err := protojson.MarshalOptions{Indent: "  "}.Marshal(m)
			if err != nil {
				return err
			}
			fmt.Println(string(out))
			return nil
		}
		// Slices of proto messages (e.g. resource lists): emit as a JSON
		// array of protojson objects.
		if reflectSlice, ok := toProtoSlice(v); ok {
			fmt.Println("[")
			for i := 0; i < reflectSlice.Len(); i++ {
				m := reflectSlice.Index(i).Interface().(proto.Message)
				out, err := protojson.MarshalOptions{Indent: "  "}.Marshal(m)
				if err != nil {
					return err
				}
				comma := ","
				if i == reflectSlice.Len()-1 {
					comma = ""
				}
				fmt.Printf("%s%s\n", string(out), comma)
			}
			fmt.Println("]")
			return nil
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case "yaml":
		out, err := marshalYAML(v)
		if err != nil {
			return err
		}
		fmt.Print(string(out))
		return nil
	default:
		table()
		return nil
	}
}

func toProtoSlice(v any) (reflect.Value, bool) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice || rv.Len() == 0 {
		return rv, false
	}
	_, ok := rv.Index(0).Interface().(proto.Message)
	return rv, ok
}

func ctlGenList(cmd *cobra.Command, opts *ctlOpts) error {
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		res, err := c.ListGenerations(ctx, &pb.ListGenerationsRequest{})
		if err != nil {
			return fmt.Errorf("ListGenerations: %w", err)
		}
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "%-6s %-20s %-8s %-12s %s\n", "GEN", "CREATED", "BY", "REVISION", "DESCRIPTION")
		for _, g := range res.Generations {
			fmt.Fprintf(w, "%-6d %-20s %-8s %-12d %s\n",
				g.Number,
				time.Unix(0, g.CreatedAtUnixNs).UTC().Format("2006-01-02T15:04:05Z"),
				g.CreatedBy, g.Revision, g.Description)
		}
		return nil
	})
}

func ctlGenShow(cmd *cobra.Command, opts *ctlOpts, arg string) error {
	n, err := strconv.ParseUint(arg, 10, 64)
	if err != nil {
		return fmt.Errorf("generation %q is not a number", arg)
	}
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		g, err := c.GetGeneration(ctx, &pb.GetGenerationRequest{Number: n, IncludeKeys: true})
		if err != nil {
			return fmt.Errorf("GetGeneration: %w", err)
		}
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "generation:  %d\n", g.Number)
		fmt.Fprintf(w, "created:     %s\n", time.Unix(0, g.CreatedAtUnixNs).UTC().Format(time.RFC3339))
		fmt.Fprintf(w, "created_by:  %s\n", g.CreatedBy)
		fmt.Fprintf(w, "revision:    %d\n", g.Revision)
		fmt.Fprintf(w, "parent:      %d\n", g.Parent)
		fmt.Fprintf(w, "hash:        %s\n", g.Hash)
		if g.Description != "" {
			fmt.Fprintf(w, "description: %s\n", g.Description)
		}
		fmt.Fprintf(w, "keys:        %d\n", len(g.Keys))
		for _, k := range g.Keys {
			fmt.Fprintf(w, "  %s\n", k)
		}
		return nil
	})
}

func ctlGenDiff(cmd *cobra.Command, opts *ctlOpts, aArg, bArg string) error {
	a, err := strconv.ParseUint(aArg, 10, 64)
	if err != nil {
		return fmt.Errorf("generation %q is not a number", aArg)
	}
	b, err := strconv.ParseUint(bArg, 10, 64)
	if err != nil {
		return fmt.Errorf("generation %q is not a number", bArg)
	}
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		d, err := c.DiffGenerations(ctx, &pb.DiffGenerationsRequest{A: a, B: b})
		if err != nil {
			return fmt.Errorf("DiffGenerations: %w", err)
		}
		w := cmd.OutOrStdout()
		for _, k := range d.Added {
			fmt.Fprintf(w, "+ %s\n", k)
		}
		for _, k := range d.Changed {
			fmt.Fprintf(w, "~ %s\n", k)
		}
		for _, k := range d.Removed {
			fmt.Fprintf(w, "- %s\n", k)
		}
		if len(d.Added)+len(d.Changed)+len(d.Removed) == 0 {
			fmt.Fprintln(w, "no differences")
		}
		return nil
	})
}

func ctlGenRollback(cmd *cobra.Command, opts *ctlOpts, args []string) error {
	var target uint64
	if len(args) == 1 {
		n, err := strconv.ParseUint(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("generation %q is not a number", args[0])
		}
		target = n
	}
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		res, err := c.RollbackGeneration(ctx, &pb.RollbackGenerationRequest{Target: target})
		if err != nil {
			return fmt.Errorf("RollbackGeneration: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "rolled back; new generation %d (history is append-only — roll forward by rolling back again)\n", res.NewGeneration)
		return nil
	})
}

func ctlNodeStatus(cmd *cobra.Command, opts *ctlOpts) error {
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		st, err := c.GetStatus(ctx, &pb.GetStatusRequest{})
		if err != nil {
			return fmt.Errorf("GetStatus: %w", err)
		}
		table := func() {
			fmt.Fprintf(cmd.OutOrStdout(), "node:        %s\n", st.NodeId)
			fmt.Fprintf(cmd.OutOrStdout(), "status:      %s\n", st.Status)
			fmt.Fprintf(cmd.OutOrStdout(), "ticks:       %d\n", st.TickCount)
			fmt.Fprintf(cmd.OutOrStdout(), "resources:   %d\n", st.ResourcesTotal)
			fmt.Fprintf(cmd.OutOrStdout(), "changes:     %d\n", st.ChangesApplied)
			fmt.Fprintf(cmd.OutOrStdout(), "failures:    %d\n", st.Failures)
			if st.LastTickUnixNs > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "last tick:   %s (%.0f ms)\n",
					time.Unix(0, st.LastTickUnixNs).Format(time.RFC3339), st.LastTickDurationMs)
			}
		}
		return emit(opts, table, st)
	})
}

func ctlNodeInspect(cmd *cobra.Command, opts *ctlOpts) error {
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		inv, err := c.GetInventory(ctx, &pb.GetInventoryRequest{})
		if err != nil {
			return fmt.Errorf("GetInventory: %w", err)
		}
		table := func() {
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "node:     %s (%s)\n", inv.NodeId, inv.Hostname)
			fmt.Fprintf(w, "os:       %s %s (kernel %s)\n", inv.Os.GetName(), inv.Os.GetVersion(), inv.Os.GetKernel())
			fmt.Fprintf(w, "cpu:      %s (%d cores / %d threads)\n", inv.Cpu.GetModel(), inv.Cpu.GetCores(), inv.Cpu.GetThreads())
			fmt.Fprintf(w, "memory:   %.1f GiB\n", float64(inv.Memory.GetTotal())/(1<<30))
			fmt.Fprintf(w, "virt:     %s\n", inv.Virtualization)
			if len(inv.Capabilities) > 0 {
				fmt.Fprintf(w, "caps:     %s\n", strings.Join(inv.Capabilities, ", "))
			}
			for _, d := range inv.Disks {
				fmt.Fprintf(w, "disk:     %s %s %.1f GiB\n", d.Path, d.Model, float64(d.Size)/(1<<30))
			}
			for _, n := range inv.Nics {
				fmt.Fprintf(w, "nic:      %s %s up=%v\n", n.Name, n.Mac, n.Up)
			}
			for _, g := range inv.Gpus {
				fmt.Fprintf(w, "gpu:      %s %s (%s)\n", g.Vendor, g.Model, g.PciId)
			}
		}
		return emit(opts, table, inv)
	})
}

func ctlNodeHealth(cmd *cobra.Command, opts *ctlOpts) error {
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		rep, err := c.GetHealth(ctx, &pb.GetHealthRequest{})
		if err != nil {
			return fmt.Errorf("GetHealth: %w", err)
		}
		table := func() {
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "overall: %s\n\n", rep.Overall)
			fmt.Fprintf(w, "%-14s %-10s %s\n", "CHECK", "STATUS", "MESSAGE")
			for _, ch := range rep.Checks {
				fmt.Fprintf(w, "%-14s %-10s %s\n", ch.Name, ch.Status, ch.Message)
			}
		}
		return emit(opts, table, rep)
	})
}

func ctlResourceList(cmd *cobra.Command, opts *ctlOpts) error {
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		resp, err := c.ListResources(ctx, &pb.ListResourcesRequest{})
		if err != nil {
			return fmt.Errorf("ListResources: %w", err)
		}
		table := func() {
			w := cmd.OutOrStdout()
			if len(resp.Resources) == 0 {
				fmt.Fprintln(w, "(no resources)")
				return
			}
			fmt.Fprintf(w, "%-40s %-14s %-10s\n", "ID", "TYPE", "HEALTH")
			for _, r := range resp.Resources {
				fmt.Fprintf(w, "%-40s %-14s %-10s\n", r.Id, r.Type, r.Health)
			}
		}
		return emit(opts, table, resp.Resources)
	})
}

func ctlResourceGet(cmd *cobra.Command, opts *ctlOpts, id string) error {
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		r, err := c.GetResource(ctx, &pb.GetResourceRequest{Id: id})
		if err != nil {
			return fmt.Errorf("GetResource: %w", err)
		}
		table := func() {
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "id:       %s\n", r.Id)
			fmt.Fprintf(w, "type:     %s\n", r.Type)
			fmt.Fprintf(w, "health:   %s\n", r.Health)
			fmt.Fprintf(w, "desired:  %s\n", strings.TrimSpace(r.DesiredState))
			fmt.Fprintf(w, "observed: %s\n", strings.TrimSpace(r.ObservedState))
		}
		return emit(opts, table, r)
	})
}

func ctlResourceApply(cmd *cobra.Command, opts *ctlOpts) error {
	f := cmd.Flag("file").Value.String()
	if f == "" {
		return fmt.Errorf("resource apply requires -f <file>")
	}
	var data []byte
	var err error
	if f == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(f)
	}
	if err != nil {
		return fmt.Errorf("read spec: %w", err)
	}
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		resp, err := c.ApplyResources(ctx, &pb.ApplyResourcesRequest{Spec: data})
		if err != nil {
			return fmt.Errorf("ApplyResources: %w", err)
		}
		for _, e := range resp.Errors {
			fmt.Fprintf(cmd.ErrOrStderr(), "error: %s\n", e)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "applied: %d  failed: %d\n", resp.Applied, resp.Failed)
		if resp.Failed > 0 {
			os.Exit(1)
		}
		return nil
	})
}

func ctlResourceDelete(cmd *cobra.Command, opts *ctlOpts, id string) error {
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		resp, err := c.DeleteResource(ctx, &pb.DeleteResourceRequest{Id: id})
		if err != nil {
			return fmt.Errorf("DeleteResource: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "deleted: %t\n", resp.Deleted)
		return nil
	})
}

func ctlShutdown(cmd *cobra.Command, opts *ctlOpts) error {
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		resp, err := c.Shutdown(ctx, &pb.ShutdownRequest{})
		if err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n", resp.Message)
		return nil
	})
}

func ctlReconcile(cmd *cobra.Command, opts *ctlOpts) error {
	dry := cmd.Flag("dry-run").Value.String() == "true"
	// Streaming can outlive the default timeout; reconcile builds can take
	// minutes. The server enforces its own tick deadline.
	ctx := cmd.Context()
	conn, err := dial(opts)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stream, err := pb.NewNodeServiceClient(conn).Reconcile(ctx, &pb.ReconcileRequest{DryRun: dry})
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reconcile stream: %w", err)
		}
		pct := ""
		if ev.ProgressPct > 0 {
			pct = fmt.Sprintf(" (%.0f%%)", ev.ProgressPct)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "[%s]%s %s\n", ev.Phase, pct, ev.Message)
	}
}

func ctlEvents(cmd *cobra.Command, opts *ctlOpts) error {
	follow := cmd.Flag("follow").Value.String() == "true"
	ctx := cmd.Context()
	if !follow {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}
	conn, err := dial(opts)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stream, err := pb.NewNodeServiceClient(conn).StreamEvents(ctx, &pb.StreamEventsRequest{})
	if err != nil {
		return fmt.Errorf("StreamEvents: %w", err)
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("events stream: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n",
			time.Unix(0, ev.TimestampUnixNs).Format(time.Kitchen), ev.Type, ev.Payload)
	}
}

// --- Node lifecycle (§4.8) --------------------------------------------
//
// These commands operate on the CLUSTER node lifecycle, so — like the
// `expanse cluster` commands — they open the node's raft-replicated
// store directly from --data-dir (the daemon must not hold the bolt
// lock, i.e. run these while expansed is stopped, or from a machine
// whose daemon is down). Mutations must run on the leader.

// ctlNodeClusterStore opens the cluster store for lifecycle commands.
func ctlNodeClusterStore(cmd *cobra.Command, dataDir, nodeID string) (*raftstore.Store, func(), error) {
	st, cleanup, err := openClusterStore(dataDir, nodeID)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
	defer cancel()
	if !control.WaitForLeader(ctx, st, 10*time.Second) {
		cleanup()
		return nil, nil, fmt.Errorf("no raft leader — cluster is degraded (§4.10)")
	}
	return st, cleanup, nil
}

// ctlNodeRequireLeader guards mutations: lifecycle writes must run on
// the leader (the CLI opens a local store; a follower's proposals
// would need forwarding this path does not provide).
func ctlNodeRequireLeader(st *raftstore.Store) error {
	if !st.IsLeader() {
		lead := st.Leader()
		hint := ""
		if lead != "" {
			hint = " (leader is " + lead + ")"
		}
		return fmt.Errorf("this node is not the raft leader%s — run `expanse ctl node …` on the leader", hint)
	}
	return nil
}

type nodeRow struct {
	ID        string
	Role      string
	Lifecycle string
	Cordoned  bool
	RaftAddr  string
	APIAddr   string
	LastSeen  string
}

func ctlNodeRows(ctx context.Context, st *raftstore.Store) ([]nodeRow, error) {
	entries, err := st.List(ctx, store.Key(join.NodesKeyPrefix))
	if err != nil {
		return nil, err
	}
	rows := make([]nodeRow, 0, len(entries))
	for _, e := range entries {
		var r join.NodeRecord
		if json.Unmarshal(e.Value, &r) != nil || r.ID == "" {
			continue
		}
		lc := r.State
		if lc == "" {
			lc = "healthy"
		}
		last := time.Unix(0, r.JoinedAt)
		degraded := false
		if se, err := st.Get(ctx, store.Key(join.NodesKeyPrefix+r.ID+"/status")); err == nil {
			last = time.Unix(0, se.UpdatedAt)
			for _, f := range strings.Fields(string(se.Value)) {
				if f == "degraded=true" {
					degraded = true
				}
			}
		}
		if degraded && !strings.Contains(lc, "degraded") {
			lc += "/degraded"
		}
		rows = append(rows, nodeRow{
			ID: r.ID, Role: r.Role, Lifecycle: lc, Cordoned: r.Cordoned,
			RaftAddr: r.RaftAddr, APIAddr: r.APIAddr,
			LastSeen: last.UTC().Format(time.RFC3339),
		})
	}
	return rows, nil
}

func newCtlNodeLifecycleCmds() []*cobra.Command {
	var dataDir, nodeID string
	flags := func(c *cobra.Command) {
		c.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
		c.Flags().StringVar(&nodeID, "node-id", "", "node ID of the local node (default: enrolled node-id)")
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List cluster nodes with lifecycle state",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, cleanup, err := ctlNodeClusterStore(cmd, dataDir, nodeID)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			rows, err := ctlNodeRows(ctx, st)
			if err != nil {
				return err
			}
			w := nodeTable()
			fmt.Fprintln(w, "ID\tROLE\tLIFECYCLE\tCORDONED\tRAFT\tAPI\tLAST-SEEN")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\t%s\t%s\n",
					r.ID, orDash(r.Role), r.Lifecycle, r.Cordoned, r.RaftAddr, orDash(r.APIAddr), r.LastSeen)
			}
			return w.Flush()
		},
	}
	flags(list)

	inspect := &cobra.Command{
		Use:   "inspect <node-id>",
		Short: "Show one node's record, status, and revocation state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, cleanup, err := ctlNodeClusterStore(cmd, dataDir, nodeID)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			id := args[0]
			e, err := st.Get(ctx, store.Key(join.NodesKeyPrefix+id))
			if err != nil {
				return fmt.Errorf("node %s: %w", id, err)
			}
			var r join.NodeRecord
			if err := json.Unmarshal(e.Value, &r); err != nil {
				return fmt.Errorf("corrupt node record: %w", err)
			}
			fmt.Printf("id:        %s\nrole:      %s\nlifecycle: %s\ncordoned:  %v\nraft:      %s\napi:       %s\njoined:    %s\n",
				r.ID, orDash(r.Role), orDash(r.State), r.Cordoned, r.RaftAddr, orDash(r.APIAddr),
				time.Unix(0, r.JoinedAt).UTC().Format(time.RFC3339))
			if se, err := st.Get(ctx, store.Key(join.NodesKeyPrefix+id+"/status")); err == nil {
				fmt.Printf("status:    %s (updated %s)\n", string(se.Value), time.Unix(0, se.UpdatedAt).UTC().Format(time.RFC3339))
			}
			if rev, err := st.Get(ctx, store.Key(join.RevokedKeyPrefix+id)); err == nil {
				fmt.Printf("REVOKED:   %s\n", string(rev.Value))
			}
			return nil
		},
	}
	flags(inspect)

	cordonCmd := func(use, short string, cordon bool) *cobra.Command {
		c := &cobra.Command{
			Use:   fmt.Sprintf("%s <node-id>", use),
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				st, cleanup, err := ctlNodeClusterStore(cmd, dataDir, nodeID)
				if err != nil {
					return err
				}
				defer cleanup()
				if err := ctlNodeRequireLeader(st); err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
				defer cancel()
				if cordon {
					return nodelc.Cordon(ctx, st, args[0])
				}
				return nodelc.Uncordon(ctx, st, args[0])
			},
		}
		flags(c)
		return c
	}

	drain := &cobra.Command{
		Use:   "drain <node-id>",
		Short: "Cordon a node and verify its resources can be re-placed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ignoreUnplaceable, _ := cmd.Flags().GetBool("ignore-unplaceable")
			st, cleanup, err := ctlNodeClusterStore(cmd, dataDir, nodeID)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := ctlNodeRequireLeader(st); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			n, err := nodelc.Drain(ctx, st, args[0], &nodelc.Options{IgnoreUnplaceable: ignoreUnplaceable})
			if err != nil {
				return err
			}
			fmt.Printf("drained %s: %d resource(s) to re-place\n", args[0], n)
			return nil
		},
	}
	drain.Flags().Bool("ignore-unplaceable", false, "drain even if some resources have nowhere else to run")
	flags(drain)

	remove := &cobra.Command{
		Use:   "remove <node-id>",
		Short: "Drain, remove from raft, and revoke a node's identity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			reason, _ := cmd.Flags().GetString("reason")
			confirmFlag, _ := cmd.Flags().GetString("confirm")
			id := args[0]

			st, cleanup, err := ctlNodeClusterStore(cmd, dataDir, nodeID)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := ctlNodeRequireLeader(st); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()

			// Interlock (§4.8): quorum-breaking removals require
			// --force AND typing the node name.
			confirm := confirmFlag
			if force && confirm == "" {
				fmt.Printf("removing %s can break the cluster. Type the node name to confirm: ", id)
				var line string
				if _, err := fmt.Scanln(&line); err != nil {
					return fmt.Errorf("confirmation required")
				}
				confirm = strings.TrimSpace(line)
			}
			if err := nodelc.Remove(ctx, st, id, &nodelc.Options{Force: force, Confirm: confirm, Reason: reason, By: st.NodeID()}); err != nil {
				return err
			}
			fmt.Printf("removed %s; its identity is revoked (re-join refused)\n", id)
			return nil
		},
	}
	remove.Flags().Bool("force", false, "permit a quorum-breaking removal (still requires typing the node name)")
	remove.Flags().String("confirm", "", "typed confirmation (non-interactive equivalent of the prompt)")
	remove.Flags().String("reason", "", "reason, recorded in the revocation")
	flags(remove)

	return []*cobra.Command{
		list, inspect, cordonCmd("cordon", "Cordon a node (no new placements)", true),
		cordonCmd("uncordon", "Uncordon a node", false), drain, remove,
	}
}

// nodeTable renders aligned CLI tables for the node lifecycle commands.
func nodeTable() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}
