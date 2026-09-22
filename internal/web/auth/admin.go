package auth

import (
	"context"
	"encoding/json"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// AdminKey is the store key holding the UI's sole admin account (D4).
// There is deliberately one operator account for this phase -- the store
// record shape (username-keyed) leaves room to grow into more without a
// migration, but multi-user isn't a Phase 2 exit criterion.
const AdminKey = "/ui/users/admin"

// AdminUsername is the fixed login name for the account at AdminKey.
const AdminUsername = "admin"

// generatedPasswordLen is the length, in random bytes, of a generated
// initial password before hex encoding (32 bytes -> 64 hex chars).
const generatedPasswordLen = 32

type adminRecord struct {
	PasswordHash string `json:"hash"`
	CreatedAt    int64  `json:"created"` // unix-nano
}

// NewAdminRecord hashes password and returns the JSON-encoded store
// record for AdminKey. Exported so `expanse ctl admin reset-password`
// can write it directly over the existing generic KV RPC
// (PutKeyValueRequest) without a bespoke gRPC method, and so both write
// paths share exactly one record encoding.
func NewAdminRecord(password string) ([]byte, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	rec := adminRecord{PasswordHash: hash, CreatedAt: time.Now().UnixNano()}
	v, err := json.Marshal(rec)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "auth.NewAdminRecord", err.Error())
	}
	return v, nil
}

// EnsureAdmin creates the admin account if it does not already exist,
// with a freshly generated random password, and returns that plaintext
// password so the caller can surface it to the operator exactly once
// (D4: analogous to how a join token is minted and shown once). It is
// safe to call from every node at startup: the store's CAS expect-absent
// write means only the node that actually creates the record gets a
// non-empty password back; every other caller (the record already
// exists) gets ("", nil) and must not print anything.
func EnsureAdmin(ctx context.Context, st store.Store) (password string, err error) {
	pw, err := randomToken(generatedPasswordLen)
	if err != nil {
		return "", err
	}
	v, err := NewAdminRecord(pw)
	if err != nil {
		return "", err
	}
	if _, err := st.CompareAndSwap(ctx, store.Key(AdminKey), 0, v); err != nil {
		if errors.Is(err, errors.KindConflict) {
			return "", nil // another node already created it; nothing to show
		}
		return "", errors.Wrap(err, errors.KindInternal, "auth.EnsureAdmin", "store write failed")
	}
	return pw, nil
}

// VerifyAdminPassword reports whether password is correct for the admin
// account. A missing account (EnsureAdmin never ran, or lost a race and
// hasn't seen the winner's write yet) is treated as "wrong password",
// never as "no auth required".
func VerifyAdminPassword(ctx context.Context, st store.Store, password string) bool {
	e, err := st.Get(ctx, store.Key(AdminKey))
	if err != nil {
		return false
	}
	var rec adminRecord
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		return false
	}
	return VerifyPassword(rec.PasswordHash, password)
}

// SetAdminPassword overwrites the admin account's password hash
// unconditionally -- the operator-initiated reset path (D4:
// `expanse ctl admin reset-password`), which needs the same local node
// trust `ctl` already assumes rather than a "forgot password" flow.
func SetAdminPassword(ctx context.Context, st store.Store, password string) error {
	v, err := NewAdminRecord(password)
	if err != nil {
		return err
	}
	if _, err := st.Put(ctx, store.Key(AdminKey), v); err != nil {
		return errors.Wrap(err, errors.KindInternal, "auth.SetAdminPassword", "store write failed")
	}
	return nil
}

// GenerateResetPassword returns a fresh random password suitable for
// `expanse ctl admin reset-password` to hash and store, and to print
// once to the operator.
func GenerateResetPassword() (string, error) {
	return randomToken(generatedPasswordLen)
}
