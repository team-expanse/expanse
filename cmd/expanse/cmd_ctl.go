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
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/expanse/expanse/proto"
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
