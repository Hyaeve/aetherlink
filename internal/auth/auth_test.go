package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
)

func TestDeriveAndVerify(t *testing.T) {
	stored, err := Derive("kiro", "correct horse battery")
	if err != nil {
		t.Fatalf("Derive returned error: %v", err)
	}
	if !stored.IsConfigured() {
		t.Fatal("derived auth should be configured")
	}
	if stored.Username != "kiro" {
		t.Fatalf("username = %q", stored.Username)
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
	if _, err := Derive("admin", "short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("Derive error = %v, want ErrPasswordTooShort", err)
	}
}

func TestDeriveRejectsEmptyUsername(t *testing.T) {
	if _, err := Derive("   ", "long-enough-password"); !errors.Is(err, ErrUsernameEmpty) {
		t.Fatalf("Derive error = %v, want ErrUsernameEmpty", err)
	}
}

func TestDeriveTrimsUsername(t *testing.T) {
	stored, err := Derive("  admin  ", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Username != "admin" {
		t.Fatalf("username = %q, want trimmed", stored.Username)
	}
}

func TestDeriveUsesFreshSalt(t *testing.T) {
	first, err := Derive("admin", "same password here")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Derive("admin", "same password here")
	if err != nil {
		t.Fatal(err)
	}
	if first.Salt == second.Salt || first.PasswordHash == second.PasswordHash {
		t.Fatal("two derivations of the same password must not be identical")
	}
}

// 内置账号必须开箱可登录，否则用户第一次打开页面就进不去。
func TestDefaultCredentialsLogIn(t *testing.T) {
	stored, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if !stored.DefaultCredentials {
		t.Fatal("Default must mark the credentials as default so the UI can nag")
	}
	if err := VerifyLogin(stored, DefaultUsername, DefaultPassword); err != nil {
		t.Fatalf("built-in admin/password should log in: %v", err)
	}
}

func TestVerifyLoginChecksUsername(t *testing.T) {
	stored, err := Derive("admin", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyLogin(stored, "admin", "long-enough-password"); err != nil {
		t.Fatalf("correct credentials rejected: %v", err)
	}
	// 用户名不区分大小写，也容忍首尾空白：NAS 上手输很容易带上。
	if err := VerifyLogin(stored, " ADMIN ", "long-enough-password"); err != nil {
		t.Fatalf("username should be case- and space-insensitive: %v", err)
	}
	if err := VerifyLogin(stored, "someone-else", "long-enough-password"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("wrong username error = %v, want ErrInvalidPassword", err)
	}
	if err := VerifyLogin(stored, "admin", "wrong-password"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("wrong password error = %v, want ErrInvalidPassword", err)
	}
}

// 旧版本的配置文件只有口令没有用户名，升级后必须还能用 admin 登录。
func TestVerifyLoginTreatsMissingUsernameAsAdmin(t *testing.T) {
	stored, err := Derive("admin", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	stored.Username = ""
	if err := VerifyLogin(stored, "admin", "long-enough-password"); err != nil {
		t.Fatalf("legacy config without a username should accept admin: %v", err)
	}
}

func TestVerifyOnUnconfiguredAuth(t *testing.T) {
	if err := Verify(config.Auth{}, "whatever"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Verify error = %v, want ErrNotConfigured", err)
	}
	if err := VerifyLogin(config.Auth{}, "admin", "whatever"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("VerifyLogin error = %v, want ErrNotConfigured", err)
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
