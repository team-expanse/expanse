package control

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// TokenInfo is one row of `cluster token list`.
type TokenInfo struct {
	Nonce   string    `json:"nonce"`
	Expires time.Time `json:"expires"`
	Uses    int       `json:"uses"`
	Max     int       `json:"max"`
	By      string    `json:"by"`
	Created time.Time `json:"created"`
}

// CreateToken mints a join token and records it (join.CreateToken
// wrapper with control-level defaults).
func CreateToken(ctx context.Context, st store.Store, clusterID string, secret []byte, ttl time.Duration, uses int, by string) (string, error) {
	if ttl <= 0 {
		ttl = join.DefaultTokenTTL
	}
	if uses <= 0 {
		uses = 1
	}
	tok, _, err := join.CreateToken(ctx, st, clusterID, secret, ttl, uses, by)
	return tok, err
}

// ListTokens returns all recorded tokens (expired ones included, so
// operators can audit).
func ListTokens(ctx context.Context, st store.Store) ([]TokenInfo, error) {
	entries, err := st.List(ctx, store.Key(join.TokenKeyPrefix))
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "control.ListTokens", "list: "+err.Error())
	}
	out := make([]TokenInfo, 0, len(entries))
	for _, e := range entries {
		var rec tokenRecordJSON
		if err := json.Unmarshal(e.Value, &rec); err != nil {
			continue
		}
		out = append(out, TokenInfo{
			Nonce:   trimSpace([]byte(string(e.Key)[len(join.TokenKeyPrefix):])),
			Expires: time.Unix(rec.Exp, 0), // seconds in the record
			Uses:    rec.Uses, Max: rec.Max, By: rec.By,
			Created: time.Unix(0, rec.Created), // unix-nano
		})
	}
	return out, nil
}

// RevokeToken deletes a token by raw token string or nonce hex.
func RevokeToken(ctx context.Context, st store.Store, secret []byte, tokenOrNonce string) error {
	nonce := tokenOrNonce
	if len(tokenOrNonce) > len(join.TokenPrefix) {
		if claims, err := join.ParseToken(tokenOrNonce, secret); err == nil {
			nonce = fmt.Sprintf("%x", claims.Nonce)
		}
	}
	entry, err := st.Get(ctx, store.Key(join.TokenKeyPrefix+nonce))
	if err != nil {
		return errors.New(errors.KindNotFound, "control.RevokeToken", "no such token")
	}
	if err := st.Delete(ctx, store.Key(join.TokenKeyPrefix+nonce), entry.Revision); err != nil {
		return errors.Wrap(err, errors.KindInternal, "control.RevokeToken", "delete: "+err.Error())
	}
	return nil
}

// tokenRecordJSON mirrors join's tokenRecord without exporting it.
type tokenRecordJSON struct {
	Exp     int64  `json:"exp"`
	Max     int    `json:"max"`
	Uses    int    `json:"uses"`
	By      string `json:"by"`
	Created int64  `json:"created"`
}
