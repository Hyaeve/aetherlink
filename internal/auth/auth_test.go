package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
)

func TestDeriveAndVerify(t *testing.T) {
	stored, err := Derive("correct horse battery")
	if err != nil {
		t.Fatalf("Derive returned error: %v", err)
	}
	if !stored.IsConfigured() {
		t.Fatal("derived auth should be configured")
	}
	if stored.Algorithm != Algorithm {
		t.Fatalf("algorithm = %q", stored.Algorithm)
	}
	if err := Verify(stored, "correct horse battery"); err != nil {
		t.Fatalf("Verify rejected the correct password: %v", err)
	}
	if err := Verify(stored, "correct horse batterz"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("Verify error = %v, want ErrInvalidPassword", err)
	}
}

func TestDeriveRejectsShortPassword(t *testing.T) {
	if _, err := Derive("short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("Derive error = %v, want ErrPasswordTooShort", err)
	}
}

func TestDeriveUsesFreshSalt(t *testing.T) {
	first, err := Derive("same password here")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Derive("same password here")
	if err != nil {
		t.Fatal(err)
	}
	if first.Salt == second.Salt || first.PasswordHash == second.PasswordHash {
		t.Fatal("two derivations of the same password must not be identical")
	}
}

func TestVerifyOnUnconfiguredAuth(t *testing.T) {
	if err := Verify(config.Auth{}, "whatever"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Verify error = %v, want ErrNotConfigured", err)
	}
}

func TestVerifyRejectsUnknownAlgorithm(t *testing.T) {
	stored := config.Auth{Algorithm: "argon2id", Salt: "c2FsdA", PasswordHash: "aGFzaA"}
	if err := Verify(stored, "whatever"); !errors.Is(err, ErrUnsupportedAlgorithm) {
		t.Fatalf("Verify error = %v, want ErrUnsupportedAlgorithm", err)
	}
}

func TestSessionStoreLifecycle(t *testing.T) {
	store := NewStore(time.Hour)
	token, expires, err := store.Issue()
	if err != nil {
		t.Fatalf("Issue returned error: %v", err)
	}
	if token == "" || !expires.After(time.Now()) {
		t.Fatalf("token = %q, expires = %v", token, expires)
	}
	if !store.Valid(token) {
		t.Fatal("freshly issued token should be valid")
	}
	if store.Valid("not-a-token") {
		t.Fatal("unknown token must be rejected")
	}
	if store.Count() != 1 {
		t.Fatalf("count = %d, want 1", store.Count())
	}
	store.Revoke(token)
	if store.Valid(token) {
		t.Fatal("revoked token should be invalid")
	}
}

func TestSessionStoreExpiresAndRevokesAll(t *testing.T) {
	store := NewStore(time.Nanosecond)
	token, _, err := store.Issue()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if store.Valid(token) {
		t.Fatal("expired token should be invalid")
	}

	store = NewStore(time.Hour)
	first, _, _ := store.Issue()
	second, _, _ := store.Issue()
	store.RevokeAll()
	if store.Valid(first) || store.Valid(second) {
		t.Fatal("RevokeAll should invalidate every session")
	}
}
