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
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/expanse/expanse/proto"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/metrics"
	webauth "github.com/expanse/expanse/internal/web/auth"
	webOIDC "github.com/expanse/expanse/internal/web/oidc"
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
	// §4.8 cluster-node lifecycle, through the running agent.
	node.AddCommand(newCtlNodeLifecycleCmds(opts)...)
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

	blockCmd, catalogCmd := newBlockCmd(opts)
	cmd.AddCommand(blockCmd)
	cmd.AddCommand(catalogCmd)
	cmd.AddCommand(newVolumeCmd(opts))
	cmd.AddCommand(newStorageCmd(opts))

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
	exp := &cobra.Command{
		Use:   "export",
		Short: "Print the current desired state as a canonical snapshot (Phase 8 X4: pipe into a backup tool's stdin)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlGenExport(cmd, opts) },
	}
	gen.AddCommand(exp)
	var importDesc string
	imp := &cobra.Command{
		Use:   "import",
		Short: "Apply a snapshot (from `generation export`, e.g. restored from a backup) as a new generation of desired state",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return ctlGenImport(cmd, opts, importDesc) },
	}
	imp.Flags().StringVar(&importDesc, "description", "", "generation description")
	gen.AddCommand(imp)
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

	// kv: raw key/value access to the node's store (§5-adjacent; the
	// VM tests' `ctl kv put/get`). Cluster mode: puts are Raft writes
	// (unavailable when degraded); gets are linearizable by default,
	// --stale serves the local FSM copy.
	kv := &cobra.Command{Use: "kv", Short: "Raw key/value access to the store"}
	var kvStale bool
	kvPut := &cobra.Command{
		Use:   "put <key> <value>",
		Short: "Write a key (Raft write in cluster mode)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				res, err := c.PutKeyValue(ctx, &pb.PutKeyValueRequest{Key: args[0], Value: []byte(args[1])})
				if err != nil {
					return fmt.Errorf("PutKeyValue: %w", err)
				}
				return emit(opts, func() { fmt.Printf("%s\n  revision: %d\n", args[0], res.GetRevision()) }, res)
			})
		},
	}
	kvGet := &cobra.Command{
		Use:   "get <key>",
		Short: "Read a key (linearizable by default; --stale for the local copy)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				res, err := c.GetKeyValue(ctx, &pb.GetKeyValueRequest{Key: args[0], Stale: kvStale})
				if err != nil {
					return fmt.Errorf("GetKeyValue: %w", err)
				}
				if !res.GetFound() {
					return fmt.Errorf("key not found: %s", args[0])
				}
				fmt.Println(string(res.GetValue()))
				return nil
			})
		},
	}
	kvGet.Flags().BoolVar(&kvStale, "stale", false, "serve the local FSM copy (works when degraded, §4.10.3)")
	kvDel := &cobra.Command{
		Use:   "delete <key>",
		Short: "Delete a key (Raft write in cluster mode)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				if _, err := c.DeleteKeyValue(ctx, &pb.DeleteKeyValueRequest{Key: args[0]}); err != nil {
					return fmt.Errorf("DeleteKeyValue: %w", err)
				}
				return nil
			})
		},
	}
	kvList := &cobra.Command{
		Use:   "list <prefix>",
		Short: "List keys under a prefix (linearizable by default; --stale for the local copy)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				res, err := c.ListKeyValue(ctx, &pb.ListKeyValueRequest{Prefix: args[0], Stale: kvStale})
				if err != nil {
					return fmt.Errorf("ListKeyValue: %w", err)
				}
				for _, e := range res.GetEntries() {
					fmt.Printf("%s\t%d\t%s\n", e.GetKey(), e.GetRevision(), e.GetValue())
				}
				return nil
			})
		},
	}
	kvList.Flags().BoolVar(&kvStale, "stale", false, "serve the local FSM copy (works when degraded, §4.10.3)")
	kv.AddCommand(kvPut, kvGet, kvDel, kvList)
	cmd.AddCommand(kv)

	// admin: the web UI's operator account (ROADMAP.md Phase 2, D4). No
	// bespoke RPC: reset-password writes the same record shape the
	// agent's own bootstrap writes, over the existing generic KV RPC,
	// at the same local-socket trust level every other `ctl` command
	// already assumes. There is deliberately no "forgot password" flow.
	adminCmd := &cobra.Command{Use: "admin", Short: "The web UI's admin account"}
	var resetPassword string
	resetPW := &cobra.Command{
		Use:   "reset-password",
		Short: "Set the web UI admin account's password, generating one if --password is omitted",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				pw := resetPassword
				if pw == "" {
					generated, err := webauth.GenerateResetPassword()
					if err != nil {
						return fmt.Errorf("generate password: %w", err)
					}
					pw = generated
				}
				rec, err := webauth.NewAdminRecord(pw)
				if err != nil {
					return fmt.Errorf("hash password: %w", err)
				}
				if _, err := c.PutKeyValue(ctx, &pb.PutKeyValueRequest{Key: webauth.AdminKey, Value: rec}); err != nil {
					return fmt.Errorf("PutKeyValue: %w", err)
				}
				if resetPassword == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "new admin password: %s\n(shown once -- save it now)\n", pw)
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "admin password updated")
				}
				return nil
			})
		},
	}
	resetPW.Flags().StringVar(&resetPassword, "password", "", "set this exact password instead of generating one (scripting use)")
	adminCmd.AddCommand(resetPW)
	cmd.AddCommand(adminCmd)

	// metrics: the Prometheus scrape bearer token (Phase 9 D2). Same
	// shape as admin reset-password -- no bespoke RPC, just the generic
	// KV RPC writing the same record shape EnsureToken's bootstrap
	// path writes, so an operator can set a known token for their own
	// Prometheus config instead of reading the auto-generated one out
	// of the journal.
	metricsCmd := &cobra.Command{Use: "metrics", Short: "The Prometheus scrape endpoint's bearer token"}
	var setToken string
	setTok := &cobra.Command{
		Use:   "set-token",
		Short: "Set the metrics scrape bearer token, generating one if --token is omitted",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				tok := setToken
				if tok == "" {
					generated, err := webauth.GenerateResetPassword()
					if err != nil {
						return fmt.Errorf("generate token: %w", err)
					}
					tok = generated
				}
				rec, err := metrics.NewTokenRecord(tok)
				if err != nil {
					return fmt.Errorf("hash token: %w", err)
				}
				if _, err := c.PutKeyValue(ctx, &pb.PutKeyValueRequest{Key: metrics.TokenKey, Value: rec}); err != nil {
					return fmt.Errorf("PutKeyValue: %w", err)
				}
				if setToken == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "new metrics token: %s\n(shown once -- save it now)\n", tok)
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "metrics token updated")
				}
				return nil
			})
		},
	}
	setTok.Flags().StringVar(&setToken, "token", "", "set this exact token instead of generating one (scripting use)")
	metricsCmd.AddCommand(setTok)
	cmd.AddCommand(metricsCmd)

	// oidc: SSO login for the web UI (Phase 10 X3). Unlike admin/metrics,
	// the client secret must be recoverable (presented at the token
	// endpoint), not just hashed, so it is sealed to the cluster secret
	// (ca.SealBytes) before this generic KV write -- the same "no bespoke
	// RPC" pattern, plus a local, lock-free read of this node's own
	// cluster-secret file (control.LoadCluster; unlike `cluster ca
	// rotate`'s raft store reopen, this never touches the raft log, so
	// it does not need the daemon stopped).
	oidcCmd := &cobra.Command{Use: "oidc", Short: "OIDC (SSO) login for the web UI"}
	var oidcIssuer, oidcClientID, oidcClientSecret, oidcRedirectURL, oidcAllow, oidcDataDir string
	oidcConfigure := &cobra.Command{
		Use:   "configure",
		Short: "Set the OIDC login configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				_, secret, _, err := control.LoadCluster(oidcDataDir)
				if err != nil {
					return fmt.Errorf("load cluster secret from %s: %w", oidcDataDir, err)
				}
				allow := splitAndTrim(oidcAllow)
				rec, err := webOIDC.NewConfigRecord(oidcIssuer, oidcClientID, oidcClientSecret, oidcRedirectURL, allow, secret)
				if err != nil {
					return err
				}
				if _, err := c.PutKeyValue(ctx, &pb.PutKeyValueRequest{Key: webOIDC.ConfigKey, Value: rec}); err != nil {
					return fmt.Errorf("PutKeyValue: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "OIDC login configured")
				return nil
			})
		},
	}
	oidcConfigure.Flags().StringVar(&oidcIssuer, "issuer", "", "OIDC issuer URL (required)")
	oidcConfigure.Flags().StringVar(&oidcClientID, "client-id", "", "OIDC client ID (required)")
	oidcConfigure.Flags().StringVar(&oidcClientSecret, "client-secret", "", "OIDC client secret (required)")
	oidcConfigure.Flags().StringVar(&oidcRedirectURL, "redirect-url", "", "the exact callback URL registered at the IdP, e.g. https://expanse-ui:8443/login/oidc/callback (required)")
	oidcConfigure.Flags().StringVar(&oidcAllow, "allow-email", "", "comma-separated list of authorized verified-email claims (required; fail closed, no default allow-all)")
	oidcConfigure.Flags().StringVar(&oidcDataDir, "data-dir", "/persist/expanse", "persistent state directory (read locally to seal --client-secret)")
	oidcCmd.AddCommand(oidcConfigure)
	cmd.AddCommand(oidcCmd)

	// lease: §4.3 singleton leases. `hold` runs the holder loop
	// server-side (renewal at TTL/3, loss detection) and streams the
	// lifecycle events with timestamps — the guard-band evidence.
	leaseCmd := &cobra.Command{Use: "lease", Short: "Singleton lease operations (§4.3)"}
	var leaseTTL string
	leaseHold := &cobra.Command{
		Use:   "hold <name>",
		Short: "Acquire and hold a lease; stream lifecycle events until it is lost",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ttl, err := parseDuration(leaseTTL)
			if err != nil {
				return fmt.Errorf("--ttl: %w", err)
			}
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				stream, err := c.HoldLease(ctx, &pb.HoldLeaseRequest{Name: args[0], TtlNs: int64(ttl)})
				if err != nil {
					return fmt.Errorf("HoldLease: %w", err)
				}
				for {
					ev, err := stream.Recv()
					if errors.Is(err, io.EOF) {
						return nil
					}
					if err != nil {
						return fmt.Errorf("HoldLease: %w", err)
					}
					fmt.Printf("%s %d\n", ev.GetPhase(), ev.GetTimestampUnixNs())
				}
			})
		},
	}
	leaseHold.Flags().StringVar(&leaseTTL, "ttl", "15s", "lease time-to-live")
	leaseHolder := &cobra.Command{
		Use:   "holder <name>",
		Short: "Show a lease's stored holder and expiry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
				res, err := c.GetLease(ctx, &pb.GetLeaseRequest{Name: args[0]})
				if err != nil {
					return fmt.Errorf("GetLease: %w", err)
				}
				if !res.GetFound() {
					fmt.Println("(none)")
					return nil
				}
				exp := "never"
				if res.GetExpiresAtUnixNs() != 0 {
					exp = time.Unix(0, res.GetExpiresAtUnixNs()).UTC().Format(time.RFC3339Nano)
				}
				fmt.Printf("holder:    %s\nexpires:   %s\nrevision:  %d\n", res.GetHolder(), exp, res.GetRevision())
				return nil
			})
		},
	}
	leaseCmd.AddCommand(leaseHold, leaseHolder)
	cmd.AddCommand(leaseCmd)
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
	output := opts.output
	if (output == "" || output == "table") && table == nil {
		output = "yaml" // single-object commands have no table form
	}
	switch output {
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

func ctlGenExport(cmd *cobra.Command, opts *ctlOpts) error {
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		res, err := c.ExportGeneration(ctx, &pb.ExportGenerationRequest{})
		if err != nil {
			return fmt.Errorf("ExportGeneration: %w", err)
		}
		_, err = cmd.OutOrStdout().Write(res.Snapshot)
		return err
	})
}

func ctlGenImport(cmd *cobra.Command, opts *ctlOpts, description string) error {
	snapshot, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return fmt.Errorf("read snapshot from stdin: %w", err)
	}
	return withClient(cmd, opts, func(ctx context.Context, c pb.NodeServiceClient) error {
		res, err := c.ImportGeneration(ctx, &pb.ImportGenerationRequest{Snapshot: snapshot, Description: description})
		if err != nil {
			return fmt.Errorf("ImportGeneration: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "imported; new generation %d\n", res.NewGeneration)
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

// splitAndTrim splits a comma-separated flag value (e.g. --allow-email)
// into trimmed, non-empty entries.
func splitAndTrim(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
