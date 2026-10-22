package rollout

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

const (
	// PlanKey holds the cluster's rollout; there is at most one.
	PlanKey = "/upgrade/plan"
	// ReportPrefix holds each node's Report, keyed by node ID.
	ReportPrefix = "/upgrade/nodes/"
	// LatestKey holds the newest release the leader found.
	LatestKey = "/upgrade/latest"
)

// Latest is the newest published release, checked daily by the leader unless Disabled.
type Latest struct {
	Version  string    `json:"version,omitempty"`
	Releases []string  `json:"releases,omitempty"` // the published releases, newest first
	Checked  time.Time `json:"checked"`
	Error    string    `json:"error,omitempty"`
	// Requested asks the leader to check now ("Check now" in the UI).
	Requested bool `json:"requested,omitempty"`
	Disabled  bool `json:"disabled,omitempty"`
}

// Available lists the published releases, newest first; a record from before the list has only Version.
func (l Latest) Available() []string {
	if len(l.Releases) == 0 && l.Version != "" {
		return []string{l.Version}
	}
	return l.Releases
}

// LatestInterval is how often the leader checks for a new release.
const LatestInterval = 24 * time.Hour

// Due reports whether the leader should check for a new release now.
func (l Latest) Due(now time.Time) bool {
	return !l.Disabled && (l.Requested || now.Sub(l.Checked) >= LatestInterval)
}

// LoadLatest returns the record, zero when none was written yet.
func LoadLatest(ctx context.Context, st store.Store) (Latest, error) {
	var l Latest
	e, err := st.Get(ctx, store.Key(LatestKey))
	if experrors.KindOf(err) == experrors.KindNotFound {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(e.Value, &l)
}

// SaveLatest writes the record.
func SaveLatest(ctx context.Context, st store.Store, l Latest) error {
	return put(ctx, st, LatestKey, l)
}

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
