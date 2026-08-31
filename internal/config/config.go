// Package config loads, validates and persists the AetherLink configuration.
//
// The config file is the single source of truth and is fully managed from the
// admin UI: the container only needs a writable /config volume. On first start
// a minimal file is written automatically, then the user sets an admin password
// and adds upstreams from the web page.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// UpstreamType identifies which media server dialect an upstream speaks.
type UpstreamType string

const (
	UpstreamAudiobookshelf UpstreamType = "audiobookshelf"
	UpstreamEmby           UpstreamType = "emby"
)

// RedirectMode decides what AetherLink does once a STRM target is known.
type RedirectMode string

const (
	// RedirectAlways answers every resolved media request with a 302.
	RedirectAlways RedirectMode = "always"
	// RedirectPrivate only redirects to RFC1918/loopback targets and streams
	// public targets through the proxy.
	RedirectPrivate RedirectMode = "private"
	// RedirectNever disables 302 and always relays bytes.
	RedirectNever RedirectMode = "never"
)

// PathMapping rewrites a path as seen by the upstream server into a path that
// exists inside the AetherLink container.
type PathMapping struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`
}

// Upstream is a single reverse proxied media server.
//
// Each upstream gets its own listening port instead of a path prefix: players
// keep talking to the same URL shape they already know, only the port changes
// from the media server's own port to the AetherLink one.
type Upstream struct {
	Name    string       `yaml:"name" json:"name"`
	Type    UpstreamType `yaml:"type" json:"type"`
	BaseURL string       `yaml:"base_url" json:"baseUrl"`
	APIKey  string       `yaml:"api_key" json:"-"`
	Enabled *bool        `yaml:"enabled,omitempty" json:"enabled"`
	// ListenPort is the container port AetherLink serves this upstream on.
	ListenPort   int          `yaml:"listen_port" json:"listenPort"`
	Insecure     bool         `yaml:"insecure_skip_verify" json:"insecureSkipVerify"`
	RedirectMode RedirectMode `yaml:"redirect_mode,omitempty" json:"redirectMode"`

	// Prefix 是已废弃的路径前缀，反代改成按端口区分后不再使用。
	// 保留这个字段只为让旧配置仍能被严格解析读进来，migrate 会清掉它，
	// 因此它永远不会再被写回磁盘。
	Prefix string `yaml:"prefix,omitempty" json:"-"`

	// StrmRoots limits which container directories .strm pointers may resolve
	// local targets into.
	StrmRoots []string `yaml:"strm_roots" json:"strmRoots"`
	// PathMappings converts upstream media paths into container paths.
	PathMappings []PathMapping `yaml:"path_mappings" json:"pathMappings"`
}

// IsEnabled reports whether the upstream should be served. Omitting the field
// means enabled.
func (u Upstream) IsEnabled() bool { return u.Enabled == nil || *u.Enabled }

// ListenAddr is the address the upstream's own reverse proxy listens on.
func (u Upstream) ListenAddr() string { return fmt.Sprintf(":%d", u.ListenPort) }

// Clone returns a deep copy so callers cannot mutate shared slices.
func (u Upstream) Clone() Upstream {
	copied := u
	if u.Enabled != nil {
		enabled := *u.Enabled
		copied.Enabled = &enabled
	}
	copied.StrmRoots = append([]string(nil), u.StrmRoots...)
	copied.PathMappings = append([]PathMapping(nil), u.PathMappings...)
	return copied
}

// Redirect holds the 302 behaviour shared by all upstreams.
type Redirect struct {
	Mode RedirectMode `yaml:"mode" json:"mode"`
	// FollowUpstreamRedirects resolves intermediate 302 hops (common for 115
	// pick-code services) before answering the client, so players that do not
	// follow redirects still get a final URL.
	FollowUpstreamRedirects bool `yaml:"follow_upstream_redirects" json:"followUpstreamRedirects"`
	MaxFollowHops           int  `yaml:"max_follow_hops" json:"maxFollowHops"`
	// ForwardUserAgent keeps the player User-Agent when talking to the STRM
	// backend. Some cloud drives bind their signed URLs to the User-Agent.
	// It is a pointer so a hand-written config that omits the key keeps the
	// default (true) instead of silently turning the feature off.
	ForwardUserAgent     *bool         `yaml:"forward_user_agent" json:"forwardUserAgent"`
	FallbackUserAgent    string        `yaml:"fallback_user_agent" json:"fallbackUserAgent"`
	BlockClientUserAgent *bool         `yaml:"block_client_user_agent" json:"blockClientUserAgent"`
	BlockedUserAgents    []string      `yaml:"blocked_user_agents,omitempty" json:"blockedUserAgents,omitempty"`
	ProbeTimeout         time.Duration `yaml:"probe_timeout" json:"probeTimeout"`
	StreamTimeout        time.Duration `yaml:"stream_timeout" json:"streamTimeout"`
	// AllowPublicTargets permits redirecting to non-private hosts.
	AllowPublicTargets *bool `yaml:"allow_public_targets" json:"allowPublicTargets"`
}

// ShouldForwardUserAgent reports the effective ForwardUserAgent value.
func (r Redirect) ShouldForwardUserAgent() bool {
	return r.ForwardUserAgent == nil || *r.ForwardUserAgent
}

func (r Redirect) ShouldBlockClientUserAgent() bool {
	return r.BlockClientUserAgent != nil && *r.BlockClientUserAgent
}

func (r Redirect) IsBlockedClientUserAgent(userAgent string) bool {
	if !r.ShouldBlockClientUserAgent() {
		return false
	}
	userAgent = strings.ToLower(strings.TrimSpace(userAgent))
	if userAgent == "" {
		return false
	}
	for _, blocked := range r.BlockedUserAgents {
		blocked = strings.ToLower(strings.TrimSpace(blocked))
		if blocked != "" && strings.Contains(userAgent, blocked) {
			return true
		}
	}
	return false
}

// PublicTargetsAllowed reports the effective AllowPublicTargets value.
func (r Redirect) PublicTargetsAllowed() bool {
	return r.AllowPublicTargets == nil || *r.AllowPublicTargets
}

// Cache tunes the resolved-target cache.
type Cache struct {
	TTL     time.Duration `yaml:"ttl" json:"ttl"`
	MaxSize int           `yaml:"max_size" json:"maxSize"`
}

// Server holds listener and logging settings.
type Server struct {
	Listen    string `yaml:"listen" json:"listen"`
	LogLevel  string `yaml:"log_level" json:"logLevel"`
	LogBuffer int    `yaml:"log_buffer" json:"logBuffer"`
	// AdminToken is an optional break-glass token accepted in addition to the
	// password login. It is normally empty and injected through
	// AETHERLINK_ADMIN_TOKEN only to recover from a forgotten password.
	AdminToken string `yaml:"admin_token,omitempty" json:"-"`
}

// Auth stores the admin password verifier. Only the derived key and its salt
// are persisted; the password itself never touches disk.
type Auth struct {
	// Username is the admin account name shown on the login page.
	Username     string `yaml:"username,omitempty" json:"username,omitempty"`
	Algorithm    string `yaml:"algorithm,omitempty" json:"algorithm,omitempty"`
	Iterations   int    `yaml:"iterations,omitempty" json:"-"`
	Salt         string `yaml:"salt,omitempty" json:"-"`
	PasswordHash string `yaml:"password_hash,omitempty" json:"-"`
	// DefaultCredentials marks that the account still uses the built-in
	// admin/password pair, so the UI can nag until it is changed.
	DefaultCredentials bool `yaml:"default_credentials,omitempty" json:"-"`
}

// IsConfigured reports whether an admin password has been set.
func (a Auth) IsConfigured() bool { return a.PasswordHash != "" && a.Salt != "" }

// Config is the root configuration document.
type Config struct {
	Server    Server     `yaml:"server" json:"server"`
	Auth      Auth       `yaml:"auth" json:"auth"`
	Redirect  Redirect   `yaml:"redirect" json:"redirect"`
	Cache     Cache      `yaml:"cache" json:"cache"`
	Upstreams []Upstream `yaml:"upstreams" json:"upstreams"`

	path string `yaml:"-"`
	// migrated 记录本次加载是否改写了旧版字段，由 Load 设置，调用方据此决定
	// 是否把迁移结果落盘。
	migrated bool `yaml:"-"`
}

// Path returns the file the config was loaded from.
func (c *Config) Path() string { return c.path }

// SetPath records where the config should be saved.
func (c *Config) SetPath(path string) { c.path = path }

// Clone returns a deep copy of the configuration so an admin request can edit a
// candidate config without touching the one currently serving traffic.
func (c *Config) Clone() *Config {
	copied := *c
	copied.Redirect.ForwardUserAgent = clonePointer(c.Redirect.ForwardUserAgent)
	copied.Redirect.BlockClientUserAgent = clonePointer(c.Redirect.BlockClientUserAgent)
	copied.Redirect.BlockedUserAgents = append([]string(nil), c.Redirect.BlockedUserAgents...)
	copied.Redirect.AllowPublicTargets = clonePointer(c.Redirect.AllowPublicTargets)
	copied.Upstreams = make([]Upstream, 0, len(c.Upstreams))
	for _, upstream := range c.Upstreams {
		copied.Upstreams = append(copied.Upstreams, upstream.Clone())
	}
	return &copied
}

// Bool wraps a literal for the optional boolean fields, which use pointers so
// that "key absent" and "key set to false" stay distinguishable.
func Bool(value bool) *bool { return &value }

func clonePointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	return Bool(*value)
}

// Default returns a configuration with production friendly defaults applied.
// It deliberately contains no upstreams: they are added from the admin UI.
func Default() *Config {
	return &Config{
		Server: Server{
			Listen:    ":5151",
			LogLevel:  "info",
			LogBuffer: 5000,
		},
		Redirect: Redirect{
			Mode:                    RedirectAlways,
			FollowUpstreamRedirects: false,
			MaxFollowHops:           5,
			ForwardUserAgent:        Bool(true),
			FallbackUserAgent:       "AetherLink",
			BlockClientUserAgent:    Bool(false),
			ProbeTimeout:            15 * time.Second,
			StreamTimeout:           0,
			AllowPublicTargets:      Bool(true),
		},
		Cache:     Cache{TTL: 2 * time.Hour, MaxSize: 4096},
		Upstreams: []Upstream{},
	}
}

// LoadOrCreate reads the config file, creating it with defaults when missing so
// a fresh container starts without any manual file preparation.
func LoadOrCreate(path string) (*Config, bool, error) {
	cfg, err := Load(path)
	if err == nil {
		return cfg, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	cfg = Default()
	cfg.path = path
	applyEnvOverrides(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, false, err
	}
	if err := cfg.Save(path); err != nil {
		return nil, false, fmt.Errorf("create %s: %w", path, err)
	}
	return cfg, true, nil
}

// Load reads a YAML config file, applies defaults and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	// Decode into a zero document so YAML nulls cannot silently wipe defaults.
	var parsed Config
	// An empty file is a legitimate state: LoadOrCreate may have just created it,
	// or the user cleared it to start over.
	if strings.TrimSpace(string(raw)) != "" {
		decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
		decoder.KnownFields(true)
		if err := decoder.Decode(&parsed); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
	}
	merge(cfg, &parsed)
	cfg.path = path
	applyEnvOverrides(cfg)
	// 旧版本用路径前缀区分上游，升级后必须先补上端口再校验，
	// 否则一份能用的老配置会让容器直接起不来。
	cfg.migrated = cfg.migrate()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Migrated 报告加载时是否改写过旧版字段，调用方应把结果保存回磁盘。
func (c *Config) Migrated() bool { return c.migrated }

// migrate 把旧版配置升级到当前结构，返回是否发生了改动。
//
// 目前只有一处：上游的 prefix 换成了独占的 listen_port。老配置没有端口，
// 这里按管理端口往上顺次分配一个空闲端口，并清掉已废弃的 prefix。
func (c *Config) migrate() bool {
	changed := false
	if c.Cache.TTL == 3*time.Hour || c.Cache.TTL == 5*time.Minute {
		c.Cache.TTL = 2 * time.Hour
		changed = true
	}
	taken := map[int]bool{}
	if adminPort := PortOf(c.Server.Listen); adminPort > 0 {
		taken[adminPort] = true
	}
	for i := range c.Upstreams {
		if port := c.Upstreams[i].ListenPort; port > 0 {
			taken[port] = true
		}
	}
	for i := range c.Upstreams {
		upstream := &c.Upstreams[i]
		if upstream.RedirectMode == "" {
			upstream.RedirectMode = c.Redirect.Mode
			changed = true
		}
		if upstream.Prefix != "" {
			upstream.Prefix = ""
			changed = true
		}
		if upstream.ListenPort > 0 {
			continue
		}
		port := nextFreePort(taken, PortOf(c.Server.Listen))
		if port == 0 {
			continue
		}
		upstream.ListenPort = port
		taken[port] = true
		changed = true
	}
	return changed
}

// nextFreePort 从 base 往上找一个还没被占用的端口，找不到返回 0。
func nextFreePort(taken map[int]bool, base int) int {
	if base <= 0 {
		base = 5151
	}
	for offset := 1; offset <= 200; offset++ {
		port := base + offset
		if port > 65535 {
			return 0
		}
		if !taken[port] {
			return port
		}
	}
	return 0
}

// merge copies non-zero values from parsed over the defaults in base.
func merge(base, parsed *Config) {
	if parsed.Server.Listen != "" {
		base.Server.Listen = parsed.Server.Listen
	}
	if parsed.Server.LogLevel != "" {
		base.Server.LogLevel = parsed.Server.LogLevel
	}
	if parsed.Server.LogBuffer != 0 {
		base.Server.LogBuffer = parsed.Server.LogBuffer
	}
	if parsed.Server.AdminToken != "" {
		base.Server.AdminToken = parsed.Server.AdminToken
	}

	base.Auth = parsed.Auth

	if parsed.Redirect.Mode != "" {
		base.Redirect.Mode = parsed.Redirect.Mode
	}
	base.Redirect.FollowUpstreamRedirects = parsed.Redirect.FollowUpstreamRedirects
	if parsed.Redirect.MaxFollowHops != 0 {
		base.Redirect.MaxFollowHops = parsed.Redirect.MaxFollowHops
	}
	if parsed.Redirect.ForwardUserAgent != nil {
		base.Redirect.ForwardUserAgent = parsed.Redirect.ForwardUserAgent
	}
	if parsed.Redirect.BlockClientUserAgent != nil {
		base.Redirect.BlockClientUserAgent = parsed.Redirect.BlockClientUserAgent
	}
	if parsed.Redirect.BlockedUserAgents != nil {
		base.Redirect.BlockedUserAgents = append([]string(nil), parsed.Redirect.BlockedUserAgents...)
	}
	if parsed.Redirect.AllowPublicTargets != nil {
		base.Redirect.AllowPublicTargets = parsed.Redirect.AllowPublicTargets
	}
	if parsed.Redirect.FallbackUserAgent != "" {
		base.Redirect.FallbackUserAgent = parsed.Redirect.FallbackUserAgent
	}
	if parsed.Redirect.ProbeTimeout != 0 {
		base.Redirect.ProbeTimeout = parsed.Redirect.ProbeTimeout
	}
	if parsed.Redirect.StreamTimeout != 0 {
		base.Redirect.StreamTimeout = parsed.Redirect.StreamTimeout
	}

	if parsed.Cache.TTL != 0 {
		base.Cache.TTL = parsed.Cache.TTL
	}
	if parsed.Cache.MaxSize != 0 {
		base.Cache.MaxSize = parsed.Cache.MaxSize
	}
	if parsed.Upstreams != nil {
		base.Upstreams = parsed.Upstreams
	}
}

// applyEnvOverrides supports a few container-level overrides. Everything else
// is managed from the admin UI and persisted to the config file.
func applyEnvOverrides(cfg *Config) {
	if value := os.Getenv("AETHERLINK_LISTEN"); value != "" {
		cfg.Server.Listen = value
	}
	if value := os.Getenv("AETHERLINK_LOG_LEVEL"); value != "" {
		cfg.Server.LogLevel = value
	}
	if value := os.Getenv("AETHERLINK_ADMIN_TOKEN"); value != "" {
		cfg.Server.AdminToken = value
	}
	if value := os.Getenv("AETHERLINK_REDIRECT_MODE"); value != "" {
		cfg.Redirect.Mode = RedirectMode(value)
	}
	if value := os.Getenv("AETHERLINK_FOLLOW_REDIRECTS"); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			cfg.Redirect.FollowUpstreamRedirects = parsed
		}
	}
}

// Validate normalizes and checks the configuration. Zero upstreams is a valid
// state: a freshly installed instance has none until the user adds one.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		return errors.New("server.listen 不能为空")
	}
	if c.Server.LogBuffer < 50 {
		c.Server.LogBuffer = 50
	}
	switch c.Redirect.Mode {
	case RedirectAlways, RedirectPrivate, RedirectNever:
	default:
		return fmt.Errorf("redirect.mode %q 必须是 always、private 或 never", c.Redirect.Mode)
	}
	if c.Redirect.MaxFollowHops < 1 {
		c.Redirect.MaxFollowHops = 1
	}
	if c.Redirect.ProbeTimeout <= 0 {
		c.Redirect.ProbeTimeout = 15 * time.Second
	}
	if strings.TrimSpace(c.Redirect.FallbackUserAgent) == "" {
		c.Redirect.FallbackUserAgent = "AetherLink"
	}
	blockedUserAgents := make([]string, 0, len(c.Redirect.BlockedUserAgents))
	for _, blocked := range c.Redirect.BlockedUserAgents {
		if blocked = strings.TrimSpace(blocked); blocked != "" {
			blockedUserAgents = append(blockedUserAgents, blocked)
		}
	}
	c.Redirect.BlockedUserAgents = blockedUserAgents
	if c.Cache.TTL < 0 {
		c.Cache.TTL = 0
	}
	if c.Cache.MaxSize <= 0 {
		c.Cache.MaxSize = 4096
	}

	adminPort := PortOf(c.Server.Listen)
	seenNames := map[string]bool{}
	seenPorts := map[int]string{}
	for i := range c.Upstreams {
		upstream := &c.Upstreams[i]
		if err := upstream.normalize(); err != nil {
			return err
		}
		if seenNames[upstream.Name] {
			return fmt.Errorf("上游名称 %q 重复", upstream.Name)
		}
		seenNames[upstream.Name] = true
		if owner, taken := seenPorts[upstream.ListenPort]; taken {
			return fmt.Errorf("上游 %s 的反代端口 %d 已被 %s 占用", upstream.Name, upstream.ListenPort, owner)
		}
		// 管理界面自己占着一个端口，上游不能抢，否则界面会被反代吞掉。
		if adminPort > 0 && upstream.ListenPort == adminPort {
			return fmt.Errorf("上游 %s 的反代端口 %d 与管理界面端口冲突，请换一个", upstream.Name, upstream.ListenPort)
		}
		seenPorts[upstream.ListenPort] = upstream.Name
	}
	return nil
}

// PortOf 从 ":5151" 或 "0.0.0.0:5151" 这类监听地址里取出端口号，取不到返回 0。
func PortOf(listen string) int {
	index := strings.LastIndex(listen, ":")
	if index < 0 {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(listen[index+1:]))
	if err != nil {
		return 0
	}
	return port
}

// normalize validates and canonicalizes a single upstream entry.
func (u *Upstream) normalize() error {
	u.Name = strings.TrimSpace(u.Name)
	if u.Name == "" {
		return errors.New("上游名称不能为空")
	}
	if strings.ContainsAny(u.Name, "/\\") {
		return fmt.Errorf("上游名称 %q 不能包含斜杠", u.Name)
	}

	switch u.Type {
	case UpstreamAudiobookshelf, UpstreamEmby:
	case "":
		return fmt.Errorf("上游 %s 缺少类型，必须是 audiobookshelf 或 emby", u.Name)
	default:
		return fmt.Errorf("上游 %s 的类型 %q 不受支持", u.Name, u.Type)
	}

	u.BaseURL = strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
	if u.BaseURL == "" {
		return fmt.Errorf("上游 %s 的地址不能为空", u.Name)
	}
	if !strings.HasPrefix(u.BaseURL, "http://") && !strings.HasPrefix(u.BaseURL, "https://") {
		return fmt.Errorf("上游 %s 的地址必须以 http:// 或 https:// 开头", u.Name)
	}

	u.APIKey = strings.TrimSpace(u.APIKey)
	if u.RedirectMode == "" {
		u.RedirectMode = RedirectAlways
	}
	switch u.RedirectMode {
	case RedirectAlways, RedirectPrivate, RedirectNever:
	default:
		return fmt.Errorf("上游 %s 的跳转模式 %q 无效", u.Name, u.RedirectMode)
	}
	if u.ListenPort == 0 {
		return fmt.Errorf("上游 %s 缺少反代端口", u.Name)
	}
	if u.ListenPort < 1 || u.ListenPort > 65535 {
		return fmt.Errorf("上游 %s 的反代端口 %d 不在 1-65535 之间", u.Name, u.ListenPort)
	}

	mappings := make([]PathMapping, 0, len(u.PathMappings))
	for _, mapping := range u.PathMappings {
		from := normalizeMappingPath(mapping.From)
		to := normalizeMappingPath(mapping.To)
		if from == "" && to == "" {
			continue
		}
		if from == "" || to == "" {
			return fmt.Errorf("上游 %s 的路径映射必须同时填写来源和目标", u.Name)
		}
		mappings = append(mappings, PathMapping{From: from, To: to})
	}
	u.PathMappings = mappings

	roots := make([]string, 0, len(u.StrmRoots))
	seen := map[string]bool{}
	for _, root := range u.StrmRoots {
		normalized := normalizeMappingPath(root)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		roots = append(roots, normalized)
	}
	u.StrmRoots = roots
	return nil
}

func normalizeMappingPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.ToSlash(path)
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}

// Save writes the configuration to disk atomically so a crash or a full volume
// cannot leave a truncated config behind.
func (c *Config) Save(path string) error {
	if path == "" {
		path = c.path
	}
	if path == "" {
		return errors.New("没有可写入的配置文件路径")
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".config-*.yaml")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// 0600 keeps the upstream API keys readable only by the service user.
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// UpstreamByPort returns the upstream serving the given container port, or nil.
func (c *Config) UpstreamByPort(port int) *Upstream {
	for i := range c.Upstreams {
		if c.Upstreams[i].ListenPort == port {
			return &c.Upstreams[i]
		}
	}
	return nil
}

// SuggestPort returns a free container port for a new upstream. It walks up from
// the admin port so the numbers stay recognisable, and skips anything already
// taken by the admin listener or another upstream.
func (c *Config) SuggestPort() int {
	candidate := PortOf(c.Server.Listen)
	if candidate <= 0 {
		candidate = 5151
	}
	for offset := 1; offset <= 200; offset++ {
		port := candidate + offset
		if port > 65535 {
			break
		}
		if c.UpstreamByPort(port) == nil {
			return port
		}
	}
	return 0
}

// UpstreamByName returns the named upstream, or nil.
func (c *Config) UpstreamByName(name string) *Upstream {
	for i := range c.Upstreams {
		if c.Upstreams[i].Name == name {
			return &c.Upstreams[i]
		}
	}
	return nil
}
