package wire

import (
	"context"
	"encoding/json"
	"net"
	"sort"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
)

// PrometheusType is the block type that scrapes the cluster's own nodes.
const PrometheusType = "monitor/prometheus"

// scrapeArgs are the --scrape-node id=host and --scrape-ca flags a
// monitor/prometheus replica needs to find and verify every node's /metrics.
func scrapeArgs(ctx context.Context, st bStore) ([]string, error) {
	entries, err := st.List(ctx, join.NodesKeyPrefix)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindOf(err), "bridge.scrapeArgs", "list nodes")
	}
	var nodes []string
	for _, e := range entries {
		var rec join.NodeRecord
		if json.Unmarshal(e.Value, &rec) != nil || rec.Role == "witness" {
			continue // witnesses serve no /metrics and run no blocks
		}
		host, _, err := net.SplitHostPort(rec.RaftAddr)
		if err != nil || host == "" {
			continue
		}
		nodes = append(nodes, rec.ID+"="+host)
	}
	sort.Strings(nodes)
	var args []string
	for _, n := range nodes {
		args = append(args, "--scrape-node", n)
	}

	trust, _, err := control.LoadCATrust(ctx, st)
	if errors.Is(err, errors.KindNotFound) {
		return args, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, errors.KindOf(err), "bridge.scrapeArgs", "load CA trust")
	}
	bundle := append([]byte{}, trust.Primary.CertPEM...)
	if trust.Outgoing != nil {
		bundle = append(bundle, trust.Outgoing.CertPEM...)
	}
	return append(args, "--scrape-ca", string(bundle)), nil
}
