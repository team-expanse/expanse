// Package auth implements the web UI's authentication: argon2id password
// hashing, store-backed sessions and the initial admin credential
// (ROADMAP.md Phase 2, D2-D4). It has no HTTP knowledge — internal/web
// wraps it with cookies and handlers.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/expanse/expanse/internal/errors"
)

// argon2id parameters. These match the RFC 9106 "moderate" recommendation
// (32 MiB, 3 passes) -- comfortably inside the whole-node budget
// (ARCHITECTURE.md §8) since login is rare, not a hot path.
const (
	argonTime    = 3
	argonMemory  = 32 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

// HashPassword returns a self-describing PHC-style encoding of password's
// argon2id hash. Never returns or logs the password itself.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New(errors.KindInternal, "auth.HashPassword", "rand: "+err.Error())
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyPassword reports whether password matches encoded, a hash
// previously produced by HashPassword. Comparison is constant-time.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
