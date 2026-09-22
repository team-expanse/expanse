package auth

import (
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("VerifyPassword rejected the correct password")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("VerifyPassword accepted an incorrect password")
	}
}

func TestHashPasswordNeverStoresPlaintext(t *testing.T) {
	hash, err := HashPassword("s3cret")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, "s3cret") {
		t.Fatalf("encoded hash contains the plaintext password: %s", hash)
	}
}

func TestVerifyPasswordRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "not-a-hash", "$argon2id$garbage"} {
		if VerifyPassword(bad, "anything") {
			t.Fatalf("VerifyPassword accepted malformed hash %q", bad)
		}
	}
}
