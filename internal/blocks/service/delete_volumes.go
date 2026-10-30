package service

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// volumeDeletes queues the storage controller's delete of every volume b's storage
// entries created, shared or per replica, and drops any still-pending creates.
func (s *Server) volumeDeletes(ctx context.Context, ns, name string, b *pb.Block) ([]store.Op, error) {
	var bases []string
	for _, st := range b.GetSpec().GetStorage() {
		bases = append(bases, storage.BlockVolumeName(ns, name, st.GetName()))
	}
	if len(bases) == 0 {
		return nil, nil
	}
	ids, err := storage.ListVolumeIDs(ctx, s.St)
	if err != nil {
		return nil, err
	}
	var ops []store.Op
	for _, id := range ids {
		spec, err := storage.LoadSpec(ctx, s.St, id)
		if err != nil || !ownedVolume(spec.Name, bases) {
			continue
		}
		raw, err := json.Marshal(map[string]string{"target": spec.Name})
		if err != nil {
			return nil, errors.Wrap(err, errors.KindInternal, "blocks.service.Delete", "volume op")
		}
		ops = append(ops, store.Op{Kind: store.OpPut, Key: store.Key("/volumes/_ops/delete/" + id), Value: raw})
	}
	pending, err := s.St.List(ctx, store.Key(storage.PendingPrefix))
	if err != nil {
		return nil, err
	}
	for _, e := range pending {
		if ownedVolume(strings.TrimPrefix(string(e.Key), storage.PendingPrefix), bases) {
			ops = append(ops, store.Op{Kind: store.OpDelete, Key: e.Key})
		}
	}
	return ops, nil
}

// ownedVolume reports whether vname is one of bases or a replica's base-<index>.
func ownedVolume(vname string, bases []string) bool {
	for _, base := range bases {
		if vname == base {
			return true
		}
		if idx, ok := strings.CutPrefix(vname, base+"-"); ok {
			if _, err := strconv.Atoi(idx); err == nil {
				return true
			}
		}
	}
	return false
}
