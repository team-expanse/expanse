package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/storage"
	pb "github.com/expanse/expanse/proto"
)

// newStorageCmd is `expanse ctl storage`: the storage-class catalog.
func newStorageCmd(opts *ctlOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "Cluster storage classes",
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

	cmd.AddCommand(classes)
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
