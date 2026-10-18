package service

import (
	"context"
	"crypto/subtle"
	"fmt"
	"sort"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/secrets"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// carrySecrets gives b the refs prev held for the fields b neither sets nor references,
// so an update that leaves a password out keeps it.
func carrySecrets(b *pb.Block, prev []*pb.SecretRef) {
	have := map[string]bool{}
	for _, r := range b.GetSpec().GetSecrets() {
		have[r.GetPath()] = true
	}
	for _, r := range prev {
		if _, set := b.GetSpec().GetConfig().GetFields()[r.GetPath()]; !set && !have[r.GetPath()] {
			b.Spec.Secrets = append(b.Spec.Secrets, r)
		}
	}
}

// sealSecrets moves b's writeOnly config values into sealed secrets referenced from
// spec.secrets ("" removes one) and returns the writes for the block's transaction.
// Replaced secrets stay until the block is deleted: a generation rollback restores
// specs that name them.
func (s *Server) sealSecrets(ctx context.Context, b *pb.Block, prev []*pb.SecretRef) ([]store.Op, error) {
	const op = "blocks.service.secrets"
	if s.Secrets == nil {
		if len(b.GetSpec().GetSecrets()) > 0 {
			return nil, errors.New(errors.KindUnavailable, op, "this node has no secret store")
		}
		return nil, nil // single-node test setups: secrets stay in the config
	}
	ns, name := b.GetMetadata().GetNamespace(), b.GetMetadata().GetName()
	refs := map[string]*pb.SecretRef{}
	for _, r := range b.GetSpec().GetSecrets() {
		refs[r.GetPath()] = r
	}
	var ops []store.Op
	written := map[string]bool{}
	for _, f := range s.writeOnly(b.GetSpec().GetType()) {
		v, ok := b.GetSpec().GetConfig().GetFields()[f]
		if !ok {
			continue
		}
		delete(b.Spec.Config.Fields, f)
		if v.GetStringValue() == "" {
			delete(refs, f)
			continue
		}
		if old := refOr(refs[f], prev, f); old != nil && s.holds(ctx, ns, old.GetName(), v.GetStringValue()) {
			refs[f] = old // unchanged: same name, so replicas keep running
			continue
		}
		n := secrets.NewName(name, f)
		put, err := s.Secrets.PutOp(ns, n, []byte(v.GetStringValue()))
		if err != nil {
			return nil, err
		}
		ops = append(ops, put)
		refs[f] = &pb.SecretRef{Name: n, Path: f}
		written[n] = true
	}
	b.Spec.Secrets = nil
	for _, r := range refs {
		if !written[r.GetName()] {
			if ok, err := s.Secrets.Exists(ctx, ns, r.GetName()); err != nil {
				return nil, errors.Wrap(err, errors.KindInternal, op, "read secret "+r.GetName())
			} else if !ok {
				return nil, errors.New(errors.KindInvalid, op, fmt.Sprintf("spec.secrets %q: no such secret in namespace %q", r.GetName(), ns))
			}
		}
		b.Spec.Secrets = append(b.Spec.Secrets, r)
	}
	sort.Slice(b.Spec.Secrets, func(i, j int) bool { return b.Spec.Secrets[i].GetPath() < b.Spec.Secrets[j].GetPath() })
	return ops, nil
}

// refOr is r, or else prev's ref for field.
func refOr(r *pb.SecretRef, prev []*pb.SecretRef, field string) *pb.SecretRef {
	for _, p := range prev {
		if r == nil && p.GetPath() == field {
			return p
		}
	}
	return r
}

// holds reports whether the stored secret name already has value v.
func (s *Server) holds(ctx context.Context, ns, name, v string) bool {
	got, err := s.Secrets.Get(ctx, ns, name)
	return err == nil && subtle.ConstantTimeCompare(got, []byte(v)) == 1
}

// dropSecrets deletes every secret the block has had (secrets.NewName prefixes them
// with the block's name).
func (s *Server) dropSecrets(ctx context.Context, ns, name string) ([]store.Op, error) {
	if s.Secrets == nil {
		return nil, nil
	}
	ents, err := s.St.List(ctx, secrets.Key(ns, name+"."))
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "blocks.service.Delete", "list secrets")
	}
	ops := make([]store.Op, 0, len(ents))
	for _, e := range ents {
		ops = append(ops, store.Op{Kind: store.OpDelete, Key: e.Key})
	}
	return ops, nil
}

func (s *Server) writeOnly(typ string) []string {
	if c := s.adCtx().Catalog; c != nil {
		return c.WriteOnlyFields(typ)
	}
	return nil
}
