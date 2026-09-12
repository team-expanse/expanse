// Package join implements cluster enrollment (spec §4.5): join tokens,
// the leader-side JoinService (:7446, TLS), and the joiner-side client.
//
// # Token format (spec §4.5, verbatim structure)
//
//	expanse-join-<base58(clusterID[0:8] || expiry || nonce || HMAC)>
//
// where expiry is 8 bytes big-endian unix seconds, nonce is 8 random
// bytes, and HMAC is HMAC-SHA256 keyed by the cluster secret over the
// preceding 24 payload bytes. The token authorizes; discovery does not
// (§4.6).
//
// # Consumption semantics (§10 "Join race on the same token")
//
// Token state lives in the cluster store at /cluster/tokens/<nonce-hex>
// as {expiry, maxUses, uses}. `token create` writes the record (CAS
// expect-absent); Join consumes it with OpCheck{record, rev}+OpPut in
// the SAME Raft txn as the /nodes/<id> write, so two racing joins with
// one single-use token serialize through Raft and exactly one wins.
// Raft's own AddVoter cannot join that atomic unit (it is a separate
// config-change log entry), so a failure between txn and AddVoter is
// rolled back with a compensating txn; a crash in between is repaired
// by the idempotent re-join path (the test for interrupted joins covers
// both directions).
package join

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// TokenPrefix is the scheme marker of every join token.
const TokenPrefix = "expanse-join-"

// DefaultTokenTTL is the default token lifetime: 15 minutes.
const DefaultTokenTTL = 15 * time.Minute

// TokenKeyPrefix is the store prefix holding token consumption records.
const TokenKeyPrefix = "/cluster/tokens/"

// NodesKeyPrefix is the store prefix holding node records.
const NodesKeyPrefix = "/nodes/"

const (
	payloadLen = 8 + 8 + 8 // clusterID[0:8] || expiry || nonce
	macLen     = sha256.Size
	tokenLen   = payloadLen + macLen
)

// TokenClaims is the decoded, verified content of a join token.
type TokenClaims struct {
	ClusterIDPrefix string // first 8 chars of the cluster ID
	Expiry          time.Time
	Nonce           []byte // 8 bytes; also the store record key
	MAC             []byte // 32 bytes
}

// tokenRecord is the consumption state of one token, stored at
// /cluster/tokens/<nonce-hex>.
type tokenRecord struct {
	ExpiryUnix int64  `json:"exp"`     // must match the token's embedded expiry
	MaxUses    int    `json:"max"`     // --uses (default 1)
	Uses       int    `json:"uses"`    // consumption count
	IssuedBy   string `json:"by"`      // issuing node ID
	CreatedAt  int64  `json:"created"` // unix-nano
}

// NodeRecord is the /nodes/<id> value written at join time.
type NodeRecord struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	RaftAddr  string `json:"raft_addr"`
	APIAddr   string `json:"api_addr"`
	JoinedAt  int64  `json:"joined_at"` // unix-nano
	Inventory string `json:"inventory,omitempty"`
}

// base58 (Bitcoin alphabet). Leading zero bytes encode as '1'.
const b58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Encode(b []byte) string {
	digits := []byte{}
	for _, byte_ := range b {
		carry := int(byte_)
		for j := range digits {
			carry += 256 * int(digits[j])
			digits[j] = byte(carry % 58)
			carry /= 58
		}
		for carry > 0 {
			digits = append(digits, byte(carry%58))
			carry /= 58
		}
	}
	for _, byte_ := range b {
		if byte_ != 0 {
			break
		}
		digits = append(digits, 0)
	}
	out := make([]byte, len(digits))
	for i, d := range digits {
		out[len(digits)-1-i] = b58Alphabet[d]
	}
	return string(out)
}

func base58Decode(s string) ([]byte, error) {
	bytes_ := []byte{}
	for _, c := range []byte(s) {
		v := -1
		for i := 0; i < 58; i++ {
			if b58Alphabet[i] == c {
				v = i
				break
			}
		}
		if v < 0 {
			return nil, errors.New(errors.KindInvalid, "join.base58", "invalid base58 character")
		}
		carry := v
		for j := range bytes_ {
			carry += 58 * int(bytes_[j])
			bytes_[j] = byte(carry % 256)
			carry /= 256
		}
		for carry > 0 {
			bytes_ = append(bytes_, byte(carry%256))
			carry /= 256
		}
	}
	// restore leading zeros
	zeros := 0
	for _, c := range []byte(s) {
		if c != '1' {
			break
		}
		zeros++
	}
	for i := 0; i < zeros; i++ {
		bytes_ = append(bytes_, 0)
	}
	// reverse
	out := make([]byte, len(bytes_))
	for i, b := range bytes_ {
		out[len(bytes_)-1-i] = b
	}
	return out, nil
}

func tokenMAC(secret []byte, payload []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(payload)
	return m.Sum(nil)
}

// CreateToken mints a join token and writes its consumption record
// (CAS expect-absent) through st. uses<=0 means single-use.
func CreateToken(ctx context.Context, st TokenWriter, clusterID string, secret []byte, ttl time.Duration, uses int, issuedBy string) (string, *TokenClaims, error) {
	if len(secret) != 32 {
		return "", nil, errors.New(errors.KindInvalid, "join.CreateToken", "cluster secret must be 32 bytes")
	}
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, errors.New(errors.KindInternal, "join.CreateToken", "rand: "+err.Error())
	}
	expiry := time.Now().Add(ttl)
	clusterPrefix := []byte(clusterID)
	if len(clusterPrefix) < 8 {
		return "", nil, errors.New(errors.KindInvalid, "join.CreateToken", "cluster ID too short")
	}
	clusterPrefix = clusterPrefix[:8]
	payload := make([]byte, 0, payloadLen)
	payload = append(payload, clusterPrefix...)
	payload = binary.BigEndian.AppendUint64(payload, uint64(expiry.Unix()))
	payload = append(payload, nonce...)
	full := append(append([]byte{}, payload...), tokenMAC(secret, payload)...)

	rec := tokenRecord{
		ExpiryUnix: expiry.Unix(),
		MaxUses:    uses,
		IssuedBy:   issuedBy,
		CreatedAt:  time.Now().UnixNano(),
	}
	if rec.MaxUses <= 0 {
		rec.MaxUses = 1
	}
	v, err := json.Marshal(rec)
	if err != nil {
		return "", nil, errors.New(errors.KindInternal, "join.CreateToken", err.Error())
	}
	key := store.Key(TokenKeyPrefix + hex.EncodeToString(nonce))
	if _, err := st.CompareAndSwap(ctx, key, 0, v); err != nil {
		return "", nil, errors.Wrap(err, errors.KindInternal, "join.CreateToken", "record: "+err.Error())
	}
	claims := &TokenClaims{
		ClusterIDPrefix: string(clusterPrefix),
		Expiry:          expiry,
		Nonce:           nonce,
		MAC:             full[payloadLen:],
	}
	return TokenPrefix + base58Encode(full), claims, nil
}

// ParseToken verifies prefix, base58 framing, HMAC and expiry. It does
// NOT consult consumption state — the service does that transactionally.
func ParseToken(tok string, secret []byte) (*TokenClaims, error) {
	if len(tok) <= len(TokenPrefix) || tok[:len(TokenPrefix)] != TokenPrefix {
		return nil, errors.New(errors.KindPermission, "join.ParseToken", "not a join token")
	}
	full, err := base58Decode(tok[len(TokenPrefix):])
	if err != nil {
		return nil, err
	}
	if len(full) != tokenLen {
		return nil, errors.New(errors.KindInvalid, "join.ParseToken", fmt.Sprintf("token length %d, want %d", len(full), tokenLen))
	}
	payload, mac := full[:payloadLen], full[payloadLen:]
	if !hmac.Equal(mac, tokenMAC(secret, payload)) {
		return nil, errors.New(errors.KindPermission, "join.ParseToken", "HMAC mismatch (tampered or wrong cluster)")
	}
	claims := &TokenClaims{
		ClusterIDPrefix: string(payload[:8]),
		Expiry:          time.Unix(int64(binary.BigEndian.Uint64(payload[8:16])), 0),
		Nonce:           append([]byte(nil), payload[16:24]...),
		MAC:             append([]byte(nil), mac...),
	}
	if time.Now().After(claims.Expiry) {
		return nil, errors.New(errors.KindPermission, "join.ParseToken", "token expired")
	}
	return claims, nil
}

// TokenWriter is the write slice of store.Store needed to persist
// token records (kept narrow for test doubles).
type TokenWriter interface {
	CompareAndSwap(ctx context.Context, k store.Key, expect store.Revision, v []byte) (store.Revision, error)
}
