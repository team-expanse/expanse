package storage

import (
	"context"
	"slices"
	"strings"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// A gone mark tells a node that a volume was deleted while the node was down: the
// record is gone from the store, so only the mark can tell the node to remove its
// replica. Absence from the store proves nothing, since a stale read would look the same.
const gonePrefix = VolumePrefix + "_gone/"

func nodeGonePrefix(node string) string { return gonePrefix + node + "/" }

// GoneKey is the store key of one node's gone mark for a volume.
func GoneKey(node, volID string) store.Key { return store.Key(nodeGonePrefix(node) + volID) }

// PutGone marks a volume as deleted from node's point of view.
func PutGone(ctx context.Context, st store.Store, node, volID string) error {
	if _, err := st.Put(ctx, GoneKey(node, volID), []byte("{}")); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "storage.PutGone", "put")
	}
	return nil
}

// ListGone returns the volumes node must remove, ordered by id.
func ListGone(ctx context.Context, st store.Store, node string) ([]string, error) {
	entries, err := st.List(ctx, store.Key(nodeGonePrefix(node)))
	if err != nil {
		return nil, experrors.Wrap(err, experrors.KindInternal, "storage.ListGone", "list")
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = strings.TrimPrefix(string(e.Key), nodeGonePrefix(node))
	}
	slices.Sort(ids)
	return ids, nil
}

// DeleteGone clears a mark once node has removed the replica.
func DeleteGone(ctx context.Context, st store.Store, node, volID string) error {
	if err := st.Delete(ctx, GoneKey(node, volID), 0); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "storage.DeleteGone", "delete")
	}
	return nil
}
