package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/storage"
	pb "github.com/expanse/expanse/proto"
)

// poolRecord is what each node's runtime publishes under
// /nodes/<id>/storage/pool (runtime.publishPoolStatus).
type poolRecord struct {
	Pool   string `json:"pool"`
	Health string `json:"health"`
	Errors uint64 `json:"errors"`
}

// newStorageCmd is `expanse ctl storage` (§4.8): pool health per node
// and the storage-class catalog.
func newStorageCmd(opts *ctlOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "Cluster storage inventory: pools and classes",
	}

	pools := &cobra.Command{
		Use:   "pools",
		Short: "zpool health per node",
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				res, err := cl.ListKeyValue(ctx, &pb.ListKeyValueRequest{Prefix: "/nodes/"})
				if err != nil {
					return err
				}
				type row struct {
					node   string
					pool   string
					health string
					errs   uint64
				}
				var rows []row
				for _, e := range res.GetEntries() {
					if !endsWith(e.GetKey(), "/storage/pool") {
						continue
					}
					node := trimPrefix(trimPrefix(string(e.Key), "/nodes/"), "/storage/pool")
					var pr poolRecord
					if json.Unmarshal(e.GetValue(), &pr) != nil {
						continue
					}
					rows = append(rows, row{node, pr.Pool, pr.Health, pr.Errors})
				}
				if len(rows) == 0 {
					fmt.Println("no pools reported (nodes without --exvol-pool publish nothing)")
					return nil
				}
				sort.Slice(rows, func(i, j int) bool { return rows[i].node < rows[j].node })
				fmt.Printf("%-24s %-16s %-10s %s\n", "NODE", "POOL", "HEALTH", "ERRORS")
				for _, r := range rows {
					fmt.Printf("%-24s %-16s %-10s %d\n", r.node, r.pool, r.health, r.errs)
				}
				return nil
			})
		},
	}

	classes := &cobra.Command{
		Use:   "classes",
		Short: "Storage-class catalog (placement knobs per class)",
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				res, err := cl.GetKeyValue(ctx, &pb.GetKeyValueRequest{Key: "/config/storage-classes"})
				if err == nil && len(res.GetValue()) > 0 {
					classes, err := storage.ParseStorageClasses(res.GetValue())
					if err != nil {
						return err
					}
					printClasses(classes)
					return nil
				}
				// No catalog configured: show the built-in default.
				printClasses([]storage.StorageClass{storage.DefaultStorageClass()})
				return nil
			})
		},
	}

	cmd.AddCommand(pools, classes)
	return cmd
}

func printClasses(classes []storage.StorageClass) {
	fmt.Printf("%-12s %-4s %s\n", "NAME", "R", "NODE SELECTOR")
	for _, cl := range classes {
		var sel []string
		for k, v := range cl.NodeSelector {
			sel = append(sel, k+"="+v)
		}
		sort.Strings(sel)
		selStr := "-"
		if len(sel) > 0 {
			selStr = joinComma(sel)
		}
		fmt.Printf("%-12s %-4d %s\n", cl.Name, cl.Replication, selStr)
	}
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func trimPrefix(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
