package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// 勾了「保持登录」的会话要扛过容器重启，靠的是磁盘上那个文件——而不是令牌本身。
// 所以这里既验「重启之后还能用」，也验「文件里没有可以直接拿去用的令牌」。
func TestRememberedSessionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store := OpenStore(time.Hour, path)
	token, expires, err := store.IssueRemembered()
	if err != nil {
		t.Fatalf("IssueRemembered returned error: %v", err)
	}
	// 有效期是固定的 7 天，跟构造 Store 时给的 ttl（1 小时）无关。
	want := time.Now().Add(DefaultRememberTTL)
	if delta := expires.Sub(want); delta > time.Minute || delta < -time.Minute {
		t.Fatalf("expires = %v, want about %v", expires, want)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("remembered session should be written to %s: %v", path, err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("the session file must not contain the token itself")
	}
	if !strings.Contains(string(raw), fingerprint(token)) {
		t.Fatal("the session file should key the session by its fingerprint")
	}

	// 模拟容器重启：同一个路径上重开一个 Store，旧令牌必须仍然有效。
	restarted := OpenStore(time.Hour, path)
	if !restarted.Valid(token) {
		t.Fatal("a remembered token should survive a restart")
	}
	if restarted.Count() != 1 {
		t.Fatalf("restored count = %d, want 1", restarted.Count())
	}
}

// 不勾「保持登录」的会话就该是「重启即失效」：它不但不能被恢复，也不该顺手往
// 盘上写一个文件出来。
func TestOrdinarySessionIsNotPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store := OpenStore(time.Hour, path)
	token, _, err := store.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary sessions must not create %s (stat err = %v)", path, err)
	}
	if OpenStore(time.Hour, path).Valid(token) {
		t.Fatal("an ordinary session must not survive a restart")
	}
}

// 「保持登录」到点就失效：文件里那条过期的记录不能被恢复回来。
func TestExpiredRememberedSessionIsNotRestored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	past := time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
	body := `{"sessions":[{"key":"` + fingerprint("stale-token") + `","expires":"` + past + `"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store := OpenStore(time.Hour, path)
	if store.Valid("stale-token") {
		t.Fatal("an expired remembered session must not come back")
	}
	if store.Count() != 0 {
		t.Fatalf("count = %d, want 0", store.Count())
	}
}

// 「保持登录」不随使用顺延：Valid 走多少次，记录的过期时间都还是签发时那一个。
// 要是随用随顺延，「维持 7 天」就变成了「只要还在用就永不过期」。
func TestRememberedSessionDoesNotSlide(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store := OpenStore(time.Hour, path)
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
	ordinary := OpenStore(time.Hour, filepath.Join(t.TempDir(), "sessions.json"))
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

// 改密码之后「谁都别再进来」：RevokeAll 得把盘上的「保持登录」一起清掉，
// 否则重启之后那条记录又把人放进来，与改密码的意图正好相反。
func TestRevokeAllClearsRememberedSessionsOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store := OpenStore(time.Hour, path)
	token, _, err := store.IssueRemembered()
	if err != nil {
		t.Fatal(err)
	}
	store.RevokeAll()
	if store.Valid(token) {
		t.Fatal("RevokeAll should invalidate the in-memory session")
	}
	if OpenStore(time.Hour, path).Valid(token) {
		t.Fatal("RevokeAll should also drop the persisted session")
	}
}

// 注销单个「保持登录」会话同样要落到盘上，不然重启之后它又活了。
func TestRevokeRememberedDropsItFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store := OpenStore(time.Hour, path)
	doomed, _, err := store.IssueRemembered()
	if err != nil {
		t.Fatal(err)
	}
	keeper, _, err := store.IssueRemembered()
	if err != nil {
		t.Fatal(err)
	}
	store.Revoke(doomed)

	restarted := OpenStore(time.Hour, path)
	if restarted.Valid(doomed) {
		t.Fatal("a revoked remembered session must not come back after a restart")
	}
	if !restarted.Valid(keeper) {
		t.Fatal("revoking one remembered session must not disturb the others")
	}
}

// 会话文件坏掉不该让服务起不来，更不该变成「默认放行」。
func TestCorruptSessionFileIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := OpenStore(time.Hour, path)
	if store.Count() != 0 {
		t.Fatalf("count = %d, want 0", store.Count())
	}
	if store.Valid("anything") {
		t.Fatal("a corrupt session file must not authenticate anybody")
	}
	// 读坏了也照样能签发新会话，界面不该因此卡住。
	token, _, err := store.IssueRemembered()
	if err != nil {
		t.Fatalf("IssueRemembered after a corrupt file returned error: %v", err)
	}
	if !store.Valid(token) {
		t.Fatal("a freshly issued remembered token should be valid")
	}
}

// 没给路径时（NewStore，或 OpenStore 传了个空白路径）「保持登录」退化成普通的
// 内存会话：能签能验，只是重启之后就没了。
func TestStoreWithoutPathKeepsRememberedSessionsInMemory(t *testing.T) {
	stores := map[string]*Store{
		"NewStore":             NewStore(time.Hour),
		"OpenStore-empty-path": OpenStore(time.Hour, "   "),
	}
	for name, store := range stores {
		token, _, err := store.IssueRemembered()
		if err != nil {
			t.Fatalf("%s: IssueRemembered returned error: %v", name, err)
		}
		if !store.Valid(token) {
			t.Fatalf("%s: a remembered token should still be valid in memory", name)
		}
	}
}
