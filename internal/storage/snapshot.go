package storage

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// SnapshotRecord says a volume has a named snapshot and which node holds it. The
// snapshot itself is a thin LV on that node only, so a restore must run there.
type SnapshotRecord struct {
	Name      string    `json:"name"`
	Node      string    `json:"node"`
	CreatedAt time.Time `json:"createdAt"`
}

var snapshotName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidSnapshotName accepts what is safe inside an LVM name and a store key.
func ValidSnapshotName(name string) error {
	if !snapshotName.MatchString(name) {
		return experrors.New(experrors.KindInvalid, "storage.ValidSnapshotName",
			"snapshot names are 1-63 lowercase letters, digits and dashes, not starting with a dash: "+name)
	}
	return nil
}

func snapshotPrefix(volID string) string { return VolumePrefix + volID + "/snapshots/" }

// SnapshotKey is the store key of one snapshot record.
func SnapshotKey(volID, name string) store.Key { return store.Key(snapshotPrefix(volID) + name) }

// PutSnapshot records a snapshot, replacing a record of the same name.
func PutSnapshot(ctx context.Context, st store.Store, volID string, r SnapshotRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "storage.PutSnapshot", "marshal")
	}
	if _, err := st.Put(ctx, SnapshotKey(volID, r.Name), raw); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "storage.PutSnapshot", "put")
	}
	return nil
}

// ListSnapshotRecords returns a volume's snapshot records ordered by name.
func ListSnapshotRecords(ctx context.Context, st store.Store, volID string) ([]SnapshotRecord, error) {
	entries, err := st.List(ctx, store.Key(snapshotPrefix(volID)))
	if err != nil {
		return nil, experrors.Wrap(err, experrors.KindInternal, "storage.ListSnapshotRecords", "list")
	}
	var out []SnapshotRecord
	for _, e := range entries {
		var r SnapshotRecord
		if err := json.Unmarshal(e.Value, &r); err != nil {
			return nil, experrors.Wrap(err, experrors.KindInternal, "storage.ListSnapshotRecords", "unmarshal "+string(e.Key))
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b SnapshotRecord) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// DeleteSnapshotRecords drops every snapshot record of a volume.
func DeleteSnapshotRecords(ctx context.Context, st store.Store, volID string) error {
	entries, err := st.List(ctx, store.Key(snapshotPrefix(volID)))
	if err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "storage.DeleteSnapshotRecords", "list")
	}
	for _, e := range entries {
		if err := st.Delete(ctx, e.Key, 0); err != nil {
			return experrors.Wrap(err, experrors.KindInternal, "storage.DeleteSnapshotRecords", "delete "+string(e.Key))
		}
	}
	return nil
}
