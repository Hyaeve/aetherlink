// Package auth 负责管理界面的口令校验与会话令牌。
//
// AetherLink 的所有配置（包括 Audiobookshelf / Emby 的 API 密钥）都保存在
// /config 下，因此管理界面必须有口令保护。磁盘上只保存 PBKDF2 派生值与随机
// 盐，明文口令不落盘、不进日志。
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
)

const (
	// Algorithm 是写入配置文件的算法标识，便于将来平滑升级参数。
	Algorithm = "pbkdf2-sha256"
	// DefaultIterations 取一个在 NAS 级 CPU 上约百毫秒量级的迭代数。
	DefaultIterations = 210000
	// MinPasswordLength 是可接受的最短口令长度。
	MinPasswordLength = 8
	// MaxUsernameLength 只是防止把整篇文章塞进用户名。
	MaxUsernameLength = 64

	// DefaultUsername / DefaultPassword 是首次启动自动写入的账号，
	// 省掉初始化向导：开箱就能登录，之后在设置页里改。
	DefaultUsername = "admin"
	DefaultPassword = "password"

	saltBytes = 16
	keyBytes  = 32
)

// DefaultSessionTTL 是登录态的默认有效期。
const DefaultSessionTTL = 12 * time.Hour

var (
	// ErrPasswordTooShort 表示口令长度不足。
	ErrPasswordTooShort = fmt.Errorf("密码至少需要 %d 个字符", MinPasswordLength)
	// ErrUsernameEmpty 表示用户名为空。
	ErrUsernameEmpty = errors.New("用户名不能为空")
	// ErrUsernameTooLong 表示用户名过长。
	ErrUsernameTooLong = fmt.Errorf("用户名不能超过 %d 个字符", MaxUsernameLength)
	// ErrNotConfigured 表示实例尚未设置管理账号。
	ErrNotConfigured = errors.New("尚未设置管理账号")
	// ErrInvalidPassword 表示账号或密码不正确。
	ErrInvalidPassword = errors.New("账号或密码不正确")
	// ErrUnsupportedAlgorithm 表示配置文件里的算法本版本无法校验。
	ErrUnsupportedAlgorithm = errors.New("配置中的口令算法不受支持")
)

// NormalizeUsername 去掉首尾空白。用户名不区分大小写，但按用户输入的原样保存。
func NormalizeUsername(username string) string { return strings.TrimSpace(username) }

// Derive 由明文账号密码生成可持久化的校验材料。
func Derive(username, password string) (config.Auth, error) {
	name := NormalizeUsername(username)
	if name == "" {
		return config.Auth{}, ErrUsernameEmpty
	}
	if len([]rune(name)) > MaxUsernameLength {
		return config.Auth{}, ErrUsernameTooLong
	}
	if len([]rune(strings.TrimSpace(password))) < MinPasswordLength {
		return config.Auth{}, ErrPasswordTooShort
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return config.Auth{}, err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, DefaultIterations, keyBytes)
	if err != nil {
		return config.Auth{}, err
	}
	return config.Auth{
		Username:     name,
		Algorithm:    Algorithm,
		Iterations:   DefaultIterations,
		Salt:         base64.RawStdEncoding.EncodeToString(salt),
		PasswordHash: base64.RawStdEncoding.EncodeToString(key),
	}, nil
}

// Default 生成内置账号 admin/password 的校验材料，并打上「仍是默认凭据」标记，
// 供界面持续提醒用户尽快修改。
func Default() (config.Auth, error) {
	derived, err := Derive(DefaultUsername, DefaultPassword)
	if err != nil {
		return config.Auth{}, err
	}
	derived.DefaultCredentials = true
	return derived, nil
}

// VerifyLogin 校验账号与密码。用户名不区分大小写；无论哪一项不对都返回同一个
// 错误，避免暴露「用户名存在但密码错」这类信息。
func VerifyLogin(stored config.Auth, username, password string) error {
	if !stored.IsConfigured() {
		return ErrNotConfigured
	}
	expected := strings.ToLower(NormalizeUsername(stored.Username))
	if expected == "" {
		// 早期版本只有口令没有用户名，按内置账号名兼容。
		expected = DefaultUsername
	}
	if subtle.ConstantTimeCompare(
		[]byte(strings.ToLower(NormalizeUsername(username))),
		[]byte(expected),
	) != 1 {
		// 仍然走一次派生，让用户名错与密码错的耗时保持一致。
		_ = Verify(stored, password)
		return ErrInvalidPassword
	}
	if err := Verify(stored, password); err != nil {
		if errors.Is(err, ErrInvalidPassword) {
			return ErrInvalidPassword
		}
		return err
	}
	return nil
}

// Verify 用恒定时间比较校验口令。
func Verify(stored config.Auth, password string) error {
	if !stored.IsConfigured() {
		return ErrNotConfigured
	}
	if stored.Algorithm != "" && stored.Algorithm != Algorithm {
		return ErrUnsupportedAlgorithm
	}
	salt, err := base64.RawStdEncoding.DecodeString(stored.Salt)
	if err != nil {
		return ErrUnsupportedAlgorithm
	}
	expected, err := base64.RawStdEncoding.DecodeString(stored.PasswordHash)
	if err != nil {
		return ErrUnsupportedAlgorithm
	}
	iterations := stored.Iterations
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	candidate, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(expected))
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(candidate, expected) != 1 {
		return ErrInvalidPassword
	}
	return nil
}

// Store 保存内存中的会话令牌。重启后所有登录态失效，这对一个自托管的管理面板
// 是可接受的取舍，也省掉了把令牌写进配置文件的风险。
type Store struct {
	mu  sync.Mutex
	ttl time.Duration
	// 键是令牌的 SHA-256，值是过期时间：即使内存被 dump 也拿不到可用令牌。
	sessions map[string]time.Time
}

// NewStore 创建会话存储，ttl <= 0 时使用 DefaultSessionTTL。
func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &Store{ttl: ttl, sessions: make(map[string]time.Time)}
}

// Issue 签发一个新令牌并返回其过期时间。
func (s *Store) Issue() (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(s.ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	s.sessions[fingerprint(token)] = expires
	return token, expires, nil
}

// Valid 判断令牌是否有效，并顺带续期，避免长时间操作中途掉线。
func (s *Store) Valid(token string) bool {
	if token == "" {
		return false
	}
	key := fingerprint(token)

	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.sessions[key]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(s.sessions, key)
		return false
	}
	s.sessions[key] = time.Now().Add(s.ttl)
	return true
}

// Revoke 注销单个令牌。
func (s *Store) Revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, fingerprint(token))
}

// RevokeAll 注销全部会话，改密码后调用。
func (s *Store) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = make(map[string]time.Time)
}

// Count 返回当前有效会话数，供状态接口展示。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	return len(s.sessions)
}

func (s *Store) evictExpiredLocked() {
	now := time.Now()
	for key, expires := range s.sessions {
		if now.After(expires) {
			delete(s.sessions, key)
		}
	}
}

func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}
