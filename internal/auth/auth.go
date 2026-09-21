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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/logx"
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

// DefaultRememberTTL 是勾选「保持登录」后登录态的有效期。
//
// 它刻意不做滑动续期：勾一次就是固定的 7 天，到点重新登录。若随用随顺延，
// 「维持 7 天」就变成「只要还在用就永不过期」，等于把一份本来会自动失效的凭据
// 变成永久凭据——与界面上那句话要表达的意思相反。
const DefaultRememberTTL = 7 * 24 * time.Hour

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

// Store 保存会话令牌。普通会话只在内存里：重启后失效，这对一个自托管的管理面板
// 是可接受的取舍，也省掉了把可用令牌写进磁盘的风险。
//
// 「保持登录」是另一类会话（见 IssueRemembered）：除内存外，还把令牌的 SHA-256
// 与过期时间落到磁盘上，因此能扛过容器重启。落盘的是哈希而不是令牌本身——
// 拿到那个文件也换不出一个能用的令牌，与「配置里只存 PBKDF2 派生值」同一个思路。
type Store struct {
	mu          sync.Mutex
	ttl         time.Duration
	rememberTTL time.Duration
	// path 是「保持登录」会话的落盘位置；为空时这类会话也只活在内存里。
	path string
	// 键是令牌的 SHA-256，值是记录：即使内存被 dump 也拿不到可用令牌。
	sessions map[string]sessionRecord
	// dirty 表示磁盘上的会话文件与内存不一致，需要重写。
	dirty bool
}

type sessionRecord struct {
	expires time.Time
	// remembered 为真表示这条会话来自「保持登录」：它落盘、且不随使用顺延。
	remembered bool
}

// NewStore 创建只存放在内存里的会话存储，ttl <= 0 时使用 DefaultSessionTTL。
func NewStore(ttl time.Duration) *Store {
	return newStore(ttl, "")
}

// OpenStore 与 NewStore 相同，只是额外把「保持登录」的会话持久化到 path，
// 并在创建时把上次运行留下的、尚未过期的那些恢复回来。
// path 为空时等价于 NewStore。
func OpenStore(ttl time.Duration, path string) *Store {
	return newStore(ttl, path)
}

func newStore(ttl time.Duration, path string) *Store {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	store := &Store{
		ttl:         ttl,
		rememberTTL: DefaultRememberTTL,
		path:        strings.TrimSpace(path),
		sessions:    make(map[string]sessionRecord),
	}
	store.load()
	return store
}

// Issue 签发一个普通令牌（只存在内存里）并返回其过期时间。
func (s *Store) Issue() (string, time.Time, error) {
	return s.issue(false)
}

// IssueRemembered 签发一个「保持登录」令牌：它会被写进磁盘上的会话文件，
// 容器重启后仍然有效，有效期是 DefaultRememberTTL。
func (s *Store) IssueRemembered() (string, time.Time, error) {
	return s.issue(true)
}

func (s *Store) issue(remembered bool) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	ttl := s.ttl
	if remembered {
		ttl = s.rememberTTL
	}
	expires := time.Now().Add(ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	s.sessions[fingerprint(token)] = sessionRecord{expires: expires, remembered: remembered}
	if remembered {
		s.dirty = true
	}
	s.flushLocked()
	return token, expires, nil
}

// Valid 判断令牌是否有效，并顺带续期，避免长时间操作中途掉线。
// 续期只对普通会话做：「保持登录」的 7 天是固定的（见 DefaultRememberTTL）。
func (s *Store) Valid(token string) bool {
	if token == "" {
		return false
	}
	key := fingerprint(token)

	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.sessions[key]
	if !ok {
		return false
	}
	if time.Now().After(record.expires) {
		delete(s.sessions, key)
		if record.remembered {
			s.dirty = true
			s.flushLocked()
		}
		return false
	}
	if !record.remembered {
		record.expires = time.Now().Add(s.ttl)
		s.sessions[key] = record
	}
	return true
}

// Revoke 注销单个令牌。
func (s *Store) Revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fingerprint(token)
	if record, ok := s.sessions[key]; ok && record.remembered {
		s.dirty = true
	}
	delete(s.sessions, key)
	s.flushLocked()
}

// RevokeAll 注销全部会话，改密码后调用。磁盘上的「保持登录」也一并清掉：
// 改密码的意思就是「谁都别再进来了」，留一条能免密进来的记录与它相悖。
func (s *Store) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = make(map[string]sessionRecord)
	s.dirty = true
	s.flushLocked()
}

// Count 返回当前有效会话数，供状态接口展示。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	s.flushLocked()
	return len(s.sessions)
}

func (s *Store) evictExpiredLocked() {
	now := time.Now()
	for key, record := range s.sessions {
		if now.After(record.expires) {
			delete(s.sessions, key)
			if record.remembered {
				s.dirty = true
			}
		}
	}
}

// persistedSession 是会话文件里的一条记录。落盘的 key 是令牌的 SHA-256 指纹，
// 而不是令牌本身：这个文件即使被人拿走，也换不出一个能用的令牌。
type persistedSession struct {
	Key     string    `json:"key"`
	Expires time.Time `json:"expires"`
}

// persistedSessions 是会话文件的顶层结构。多包一层对象，是为了将来能加版本号之类
// 的字段而不至于让旧文件的解析彻底失效。
type persistedSessions struct {
	Sessions []persistedSession `json:"sessions"`
}

// load 恢复上次运行留下来的、尚未过期的「保持登录」会话。
//
// 读不动不算致命错误：大不了让用户重新登录一次，不该因此让服务起不来，所以这里
// 只记日志、不返回错误。普通会话本来就不落盘，自然也不会被恢复。
func (s *Store) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logx.Warnf("[auth] 读取会话文件 %s 失败: %v", s.path, err)
		}
		return
	}
	var stored persistedSessions
	if err := json.Unmarshal(data, &stored); err != nil {
		logx.Warnf("[auth] 解析会话文件 %s 失败: %v", s.path, err)
		return
	}
	now := time.Now()
	restored := 0
	for _, item := range stored.Sessions {
		if item.Key == "" || now.After(item.Expires) {
			continue
		}
		s.sessions[item.Key] = sessionRecord{expires: item.Expires, remembered: true}
		restored++
	}
	if restored > 0 {
		logx.Infof("[auth] 已恢复 %d 个「保持登录」会话", restored)
	}
	if restored != len(stored.Sessions) {
		// 文件里还留着已经过期的记录，标脏，下次写入时顺手清掉。
		s.dirty = true
	}
}

// flushLocked 把「保持登录」会话写回磁盘，只在内存脏了的时候才写。
//
// 先写同目录下的临时文件、再改名覆盖：容器在写一半时被杀掉也不会留下半个文件——
// 那会让下次启动解析失败，把所有还应该有效的「保持登录」一起弄丢。
// 调用方必须已持有 s.mu。
func (s *Store) flushLocked() {
	if s.path == "" || !s.dirty {
		return
	}
	now := time.Now()
	stored := persistedSessions{Sessions: make([]persistedSession, 0, len(s.sessions))}
	for key, record := range s.sessions {
		if !record.remembered || now.After(record.expires) {
			continue
		}
		stored.Sessions = append(stored.Sessions, persistedSession{Key: key, Expires: record.expires})
	}
	// 按过期时间排一下序，文件内容就不会随 map 的遍历顺序抖动，人工翻看也顺眼。
	sort.Slice(stored.Sessions, func(i, j int) bool {
		return stored.Sessions[i].Expires.Before(stored.Sessions[j].Expires)
	})
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		logx.Warnf("[auth] 序列化会话失败: %v", err)
		return
	}
	data = append(data, '\n')

	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		logx.Warnf("[auth] 创建会话文件目录 %s 失败: %v", directory, err)
		return
	}
	temporary, err := os.CreateTemp(directory, ".sessions-*.tmp")
	if err != nil {
		logx.Warnf("[auth] 创建会话临时文件失败: %v", err)
		return
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(data); err == nil {
		err = temporary.Close()
	} else {
		_ = temporary.Close()
	}
	if err != nil {
		_ = os.Remove(temporaryPath)
		logx.Warnf("[auth] 写入会话临时文件失败: %v", err)
		return
	}
	// 0600：文件里放的是会话指纹，没必要让同机上的其他账号读到。
	// CreateTemp 本来就用 0600 创建，这里显式再设一次，免得受 umask 之类影响。
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		_ = os.Remove(temporaryPath)
		logx.Warnf("[auth] 设置会话文件权限失败: %v", err)
		return
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		_ = os.Remove(temporaryPath)
		logx.Warnf("[auth] 保存会话文件 %s 失败: %v", s.path, err)
		return
	}
	s.dirty = false
}

func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}
