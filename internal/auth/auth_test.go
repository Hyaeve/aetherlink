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

// 「保持登录」只放宽有效期，不放宽作用范围：它让**签发它的那个浏览器**在 7 天内
// 不必重新登录，前提是容器没重启过。重启之后所有会话一起失效 —— 这是用户
// 2026-09-22 明确要的边界（此前有一版把指纹落盘到 sessions.json 扛重启，已撤掉）。
//
// 「重启」在这里由「另起一个 Store」代表：现在没有任何会话离开过内存，所以
// 「新 Store 认不出旧令牌」与「容器重启后要重新登录」是同一件事。
func TestRememberedSessionDoesNotSurviveRestart(t *testing.T) {
	store := NewStore(time.Hour)
	token, expires, err := store.IssueRemembered()
	if err != nil {
		t.Fatalf("IssueRemembered returned error: %v", err)
	}
	// 有效期是固定的 7 天，跟构造 Store 时给的 ttl（1 小时）无关。
	want := time.Now().Add(DefaultRememberTTL)
	if delta := expires.Sub(want); delta > time.Minute || delta < -time.Minute {
		t.Fatalf("expires = %v, want about %v", expires, want)
	}
	if !store.Valid(token) {
		t.Fatal("容器还在跑的时候，「保持登录」的令牌必须是有效的")
	}

	// 模拟容器重启。
	restarted := NewStore(time.Hour)
	if restarted.Valid(token) {
		t.Fatal("容器重启后「保持登录」也必须失效，不能靠旧令牌免登录进来")
	}
	if restarted.Count() != 0 {
		t.Fatalf("restarted count = %d, want 0", restarted.Count())
	}
}

// 「保持登录」到点就失效：7 天是硬期限，不是「只要还在用就一直续」。7 天等不起，
// 所以直接把记录里的过期时间改到过去 —— 对 Valid 来说这与「时间真的到了」等价。
func TestExpiredRememberedSessionIsRejected(t *testing.T) {
	store := NewStore(time.Hour)
	token, _, err := store.IssueRemembered()
	if err != nil {
		t.Fatal(err)
	}
	key := fingerprint(token)
	store.mu.Lock()
	record := store.sessions[key]
	record.expires = time.Now().Add(-time.Minute)
	store.sessions[key] = record
	store.mu.Unlock()

	if store.Valid(token) {
		t.Fatal("过期的「保持登录」不该还算有效")
	}
	if store.Count() != 0 {
		t.Fatalf("count = %d, want 0（过期记录要被顺手清掉）", store.Count())
	}
}

// 「保持登录」不随使用顺延：Valid 走多少次，记录的过期时间都还是签发时那一个。
// 要是随用随顺延，「维持 7 天」就变成了「只要还在用就永不过期」。
func TestRememberedSessionDoesNotSlide(t *testing.T) {
	store := NewStore(time.Hour)
	token, _, err := store.IssueRemembered()
	if err != nil {
		t.Fatal(err)
	}
	key := fingerprint(token)
	store.mu.Lock()
	issued := store.sessions[key].expires
	store.mu.Unlock()

	time.Sleep(2 * time.Millisecond)
	if !store.Valid(token) {
		t.Fatal("a remembered token should stay valid")
	}
	store.mu.Lock()
	after := store.sessions[key].expires
	store.mu.Unlock()
	if !after.Equal(issued) {
		t.Fatalf("expires moved from %v to %v: 保持登录的 7 天不该随使用顺延", issued, after)
	}

	// 对照组：普通会话就该顺延，否则长时间操作中途会掉线。
	ordinary := NewStore(time.Hour)
	ordinaryToken, _, err := ordinary.Issue()
	if err != nil {
		t.Fatal(err)
	}
	ordinaryKey := fingerprint(ordinaryToken)
	ordinary.mu.Lock()
	issuedOrdinary := ordinary.sessions[ordinaryKey].expires
	ordinary.mu.Unlock()

	time.Sleep(2 * time.Millisecond)
	if !ordinary.Valid(ordinaryToken) {
		t.Fatal("an ordinary token should stay valid")
	}
	ordinary.mu.Lock()
	afterOrdinary := ordinary.sessions[ordinaryKey].expires
	ordinary.mu.Unlock()
	if !afterOrdinary.After(issuedOrdinary) {
		t.Fatalf("expires = %v, want later than %v for an ordinary session", afterOrdinary, issuedOrdinary)
	}
}

// 注销「保持登录」的会话要立刻生效，而且不牵连别人；改密码走的 RevokeAll 连它一并清掉。
func TestRevokeRememberedSessionOnlyDropsThatOne(t *testing.T) {
	store := NewStore(time.Hour)
	doomed, _, err := store.IssueRemembered()
	if err != nil {
		t.Fatal(err)
	}
	keeper, _, err := store.IssueRemembered()
	if err != nil {
		t.Fatal(err)
	}
	store.Revoke(doomed)

	if store.Valid(doomed) {
		t.Fatal("注销掉的「保持登录」不该还能用")
	}
	if !store.Valid(keeper) {
		t.Fatal("注销一条不该牵连另一条")
	}
	if store.Count() != 1 {
		t.Fatalf("count = %d, want 1", store.Count())
	}

	// 改密码的意思就是「谁都别再进来」。
	store.RevokeAll()
	if store.Valid(keeper) {
		t.Fatal("RevokeAll 之后「保持登录」也必须失效")
	}
	if store.Count() != 0 {
		t.Fatalf("count = %d, want 0", store.Count())
	}
}

// 这里不再有「会话文件」相关的用例：会话已经不落盘了（见 Store 的注释）。
// 「重启之后认不出旧令牌」由 TestRememberedSessionDoesNotSurviveRestart 钉住；
// 「一个文件都不写」由 adminapi 的 TestRememberedSessionDoesNotWriteToDisk 端到端
// 钉住 —— 那一层才拿得到配置目录，能真的去数目录里有什么。
