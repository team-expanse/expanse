package rollout

import (
	"context"
	"encoding/json"
	"strings"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

const (
	// PlanKey holds the cluster's rollout; there is at most one.
	PlanKey = "/upgrade/plan"
	// ReportPrefix holds each node's Report, keyed by node ID.
	ReportPrefix = "/upgrade/nodes/"
)

// LoadPlan returns the rollout, or nil when none was ever started.
func LoadPlan(ctx context.Context, st store.Store) (*Plan, error) {
	e, err := st.Get(ctx, store.Key(PlanKey))
	if experrors.KindOf(err) == experrors.KindNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(e.Value, &p); err != nil {
		return nil, experrors.Wrap(err, experrors.KindInternal, "rollout", "corrupt record "+PlanKey)
	}
	return &p, nil
}

// SavePlan writes the rollout.
func SavePlan(ctx context.Context, st store.Store, p Plan) error {
	return put(ctx, st, PlanKey, p)
}

// SaveReport writes a node's report.
func SaveReport(ctx context.Context, st store.Store, node string, r Report) error {
	return put(ctx, st, ReportPrefix+node, r)
}

// ListReports maps each reporting node to its Report, skipping unreadable records.
func ListReports(ctx context.Context, st store.Store) (map[string]Report, error) {
	ents, err := st.List(ctx, store.Key(ReportPrefix))
	if err != nil {
		return nil, err
	}
	out := make(map[string]Report, len(ents))
	for _, e := range ents {
		var r Report
		if json.Unmarshal(e.Value, &r) == nil {
			out[strings.TrimPrefix(string(e.Key), ReportPrefix)] = r
		}
	}
	return out, nil
}

func put(ctx context.Context, st store.Store, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = st.Put(ctx, store.Key(key), raw)
	return err
}
