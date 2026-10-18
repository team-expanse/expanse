// Package secrets keeps block passwords and tokens out of block specs: each value is
// sealed with the cluster secret (ca.SealBytes) and stored at /secrets/<ns>/<name>.
package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// Prefix is where sealed secrets live; it is not a generation (desired-state) prefix.
const Prefix = "/secrets/"

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// ValidName reports whether n can name a secret (one store key segment).
func ValidName(n string) bool { return nameRE.MatchString(n) }

// Key is the store key of a secret.
func Key(ns, name string) store.Key { return store.Key(Prefix + ns + "/" + name) }

// NewName names a fresh secret for a block's field; the random suffix makes every
// new value a new name, so the block's spec changes and its replicas restart.
func NewName(block, field string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return block + "." + field + "." + hex.EncodeToString(b)
}

// Store seals and unseals secrets with the cluster secret.
type Store struct {
	St            store.Store
	ClusterSecret []byte
}

// PutOp seals value into a write of the secret, for a transaction with the block.
func (s *Store) PutOp(ns, name string, value []byte) (store.Op, error) {
	const op = "secrets.Put"
	if len(s.ClusterSecret) == 0 {
		return store.Op{}, errors.New(errors.KindUnavailable, op, "no cluster secret to seal with")
	}
	sealed, err := ca.SealBytes(value, s.ClusterSecret)
	if err != nil {
		return store.Op{}, errors.Wrap(err, errors.KindInternal, op, "seal "+name)
	}
	return store.Op{Kind: store.OpPut, Key: Key(ns, name), Value: sealed}, nil
}

// DeleteOp removes a secret, for a transaction with the block.
func DeleteOp(ns, name string) store.Op {
	return store.Op{Kind: store.OpDelete, Key: Key(ns, name)}
}

// Get unseals a secret; a missing one is KindNotFound.
func (s *Store) Get(ctx context.Context, ns, name string) ([]byte, error) {
	const op = "secrets.Get"
	e, err := s.St.Get(ctx, Key(ns, name))
	if err != nil {
		if errors.Is(err, errors.KindNotFound) {
			return nil, errors.New(errors.KindNotFound, op, "secret "+ns+"/"+name)
		}
		return nil, errors.Wrap(err, errors.KindInternal, op, "read "+name)
	}
	v, err := ca.UnsealBytes(e.Value, s.ClusterSecret)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, op, "unseal "+name)
	}
	return v, nil
}

// Exists reports whether a secret is stored, without unsealing it.
func (s *Store) Exists(ctx context.Context, ns, name string) (bool, error) {
	_, err := s.St.Get(ctx, Key(ns, name))
	if errors.Is(err, errors.KindNotFound) {
		return false, nil
	}
	return err == nil, err
}
