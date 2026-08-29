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

	saltBytes = 16
	keyBytes  = 32
)

// DefaultSessionTTL 是登录态的默认有效期。
const DefaultSessionTTL = 12 * time.Hour

var (
	// ErrPasswordTooShort 表示口令长度不足。
	ErrPasswordTooShort = fmt.Errorf("管理口令至少需要 %d 个字符", MinPasswordLength)
	// ErrNotConfigured 表示实例尚未设置管理口令。
	ErrNotConfigured = errors.New("尚未设置管理口令")
	// ErrInvalidPassword 表示口令不正确。
	ErrInvalidPassword = errors.New("口令不正确")
	// ErrUnsupportedAlgorithm 表示配置文件里的算法本版本无法校验。
	ErrUnsupportedAlgorithm = errors.New("配置中的口令算法不受支持")
)

// Derive 由明文口令生成可持久化的校验材料。
func Derive(password string) (config.Auth, error) {
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
		Algorithm:    Algorithm,
		Iterations:   DefaultIterations,
		Salt:         base64.RawStdEncoding.EncodeToString(salt),
		PasswordHash: base64.RawStdEncoding.EncodeToString(key),
	}, nil
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
