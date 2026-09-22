package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// SessionKeyPrefix is the store prefix holding session records. Sessions
// live in the (Raft-replicated) cluster store, not process memory, so a
// cookie issued by one node authorizes a request answered by another --
// required for the UI to survive its VIP failing over (D2, X7).
const SessionKeyPrefix = "/ui/sessions/"

// DefaultSessionTTL is how long an issued session stays valid.
const DefaultSessionTTL = 24 * time.Hour

// Session is a logged-in operator's session record.
type Session struct {
	ID        string `json:"-"` // the cookie value; not stored inside the record itself
	UserID    string `json:"user"`
	CSRFToken string `json:"csrf"`
	ExpiresAt int64  `json:"exp"`     // unix seconds
	CreatedAt int64  `json:"created"` // unix-nano
}

func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New(errors.KindInternal, "auth.randomToken", "rand: "+err.Error())
	}
	return hex.EncodeToString(b), nil
}

// IssueSession creates a new session for userID with DefaultSessionTTL and
// persists it. The returned Session.ID is the opaque, unguessable cookie
// value; nothing about it reveals the user or CSRF token to a browser
// that doesn't already hold the cookie.
func IssueSession(ctx context.Context, st store.Store, userID string) (*Session, error) {
	id, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	sess := &Session{
		ID:        id,
		UserID:    userID,
		CSRFToken: csrf,
		ExpiresAt: now.Add(DefaultSessionTTL).Unix(),
		CreatedAt: now.UnixNano(),
	}
	v, err := json.Marshal(sess)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "auth.IssueSession", err.Error())
	}
	// expect-absent: a collision in 32 random bytes is not a real
	// possibility, but CAS costs nothing and keeps the invariant honest.
	if _, err := st.CompareAndSwap(ctx, store.Key(SessionKeyPrefix+id), 0, v); err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "auth.IssueSession", "store write failed")
	}
	return sess, nil
}

// GetSession looks up id, returning errors.KindNotFound if it does not
// exist or has expired. An expired record is deleted as a side effect.
func GetSession(ctx context.Context, st store.Store, id string) (*Session, error) {
	if id == "" {
		return nil, errors.New(errors.KindNotFound, "auth.GetSession", "no session ID")
	}
	e, err := st.Get(ctx, store.Key(SessionKeyPrefix+id))
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(e.Value, &sess); err != nil {
		return nil, errors.New(errors.KindInternal, "auth.GetSession", "corrupt session record: "+err.Error())
	}
	sess.ID = id
	if time.Now().Unix() >= sess.ExpiresAt {
		_ = st.Delete(ctx, store.Key(SessionKeyPrefix+id), 0)
		return nil, errors.New(errors.KindNotFound, "auth.GetSession", "session expired")
	}
	return &sess, nil
}

// InvalidateSession removes id unconditionally (logout). Deleting an
// already-gone session is not an error.
func InvalidateSession(ctx context.Context, st store.Store, id string) error {
	if id == "" {
		return nil
	}
	err := st.Delete(ctx, store.Key(SessionKeyPrefix+id), 0)
	if errors.Is(err, errors.KindNotFound) {
		return nil
	}
	return err
}
