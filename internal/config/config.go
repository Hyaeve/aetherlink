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
	"net/netip"
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
	// UpstreamFnos 是飞牛 NAS 自带的「影视」应用。它本身就是一套 Emby 方言的
	// 服务端（接口挂在 /emby 前缀下，播放路由与 PlaybackInfo 字段与 Emby 一致），
	// 所以反代与解析都复用 Emby 的一套逻辑，只在端点前缀与网页播放器上做适配。
	UpstreamFnos UpstreamType = "fnos"
)

// IsEmbyFamily 报告该类型是否属于 Emby 方言家族（Emby 本身与飞牛影视）。
// 播放协商改写、HLS 判断、UA 屏蔽名单、缓存 TTL 都按这个家族统一处理。
func (t UpstreamType) IsEmbyFamily() bool {
	return t == UpstreamEmby || t == UpstreamFnos
}

// RedirectMode decides what AetherLink does once a STRM target is known.
type RedirectMode string

const (
	// RedirectAlways answers every resolved remote media request with a 302.
	RedirectAlways RedirectMode = "always"
	// RedirectPublic redirects public clients and relays private clients.
	RedirectPublic RedirectMode = "public"
	// RedirectPrivate redirects private clients and relays public clients.
	RedirectPrivate RedirectMode = "private"
	// RedirectNever disables 302 and always relays bytes.
	RedirectNever RedirectMode = "never"
)

// RedirectModeLabel 返回卡片上显示的档位名。日志里只写 `never` 这类配置取值时，
// 用户对着界面上的「始终中继」根本对不上，曾经因此把一行中继日志读成了别的档位；
// 名字与前端 `redirectOptions` 保持一致，改动要一起改。
func RedirectModeLabel(mode RedirectMode) string {
	switch mode {
	case RedirectAlways:
		return "始终跳转"
	case RedirectPublic:
		return "公网跳转"
	case RedirectPrivate:
		return "内网跳转"
	case RedirectNever:
		return "始终中继"
	default:
		return string(mode)
	}
}

// RedirectModeName 把档位名与配置取值拼成一个可直接写进日志的词，例如
// 「始终中继（never）」：中文名让用户对得上界面，括号里的原值让排障时能直接
// grep 配置与文档。
func RedirectModeName(mode RedirectMode) string {
	if label := RedirectModeLabel(mode); label != string(mode) {
		return fmt.Sprintf("%s（%s）", label, mode)
	}
	return string(mode)
}

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
	// Username / Password 是「没有静态密钥、只能账号密码登录」的上游的凭据，
	// 目前只有飞牛影视用得到：它没有 Emby 控制台里那种 API 密钥，接口要求先
	// 以某个用户身份登录换取访问令牌。两项必须同时填写；都留空表示 AetherLink
	// 完全不调用它的 API —— 此时 Emby 系依旧能靠 PlaybackInfo 改写加缓存完成
	// 302，只是拿不到媒体库列表、也失去了缓存未命中时的兜底解析。
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	Password string `yaml:"password,omitempty" json:"-"`
	Enabled  *bool  `yaml:"enabled,omitempty" json:"enabled"`
	// ListenPort is the container port AetherLink serves this upstream on.
	ListenPort   int          `yaml:"listen_port" json:"listenPort"`
	Insecure     bool         `yaml:"insecure_skip_verify" json:"insecureSkipVerify"`
	RedirectMode RedirectMode `yaml:"redirect_mode,omitempty" json:"redirectMode"`
	// RelayExemptUserAgents 列出「别给它们中继」的客户端 UA。命中者即使本卡片选
	// 的是「始终中继」，也把直链 302 出去（等价于让反代工具自己跟随重定向后代理，
	// 也就是这批播放器唯一能播的形态）。
	//
	// 存在的理由：中继与 302 交给播放器的字节、响应头已被逐项比对为等价，但仍有
	// 播放器（AfuseKt 那一系）只在「拿着直链自己取流」时能播，走我们中继就只读几
	// KB 就撒手。这是客户端的选路差异，不是中继能修的，所以把选择权交回用户：
	// 把那个 UA 填进来，它单独走直链，其余客户端照旧中继。
	//
	// 匹配规则与「屏蔽 UA」名单一致：大小写不敏感的子串。
	RelayExemptUserAgents []string `yaml:"relay_exempt_user_agents,omitempty" json:"relayExemptUserAgents,omitempty"`

	// IgnoreDirectPlayVerdict 是已废弃的开关。它曾经让「上游说这个客户端不能
	// 直接播放原始文件」那句判定变得可听可不听；后来四种跳转模式一律忽略那句
	// 判定，没有可关的余地，这个开关就只剩「关掉之后让一部分客户端播不了」这
	// 一个作用，于是连同界面一起删掉了。
	//
	// 与 Prefix 同理：声明保留只为让已经写过它的旧配置仍能通过严格解析读进来，
	// migrate 会把它清掉，因此它永远不会再被写回磁盘。
	IgnoreDirectPlayVerdict *bool `yaml:"ignore_direct_play_verdict,omitempty" json:"-"`

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

// RelayExempt reports whether this client must be kept off the relay path and
// handed the direct link instead. Matching mirrors the "blocked UA" lists:
// case-insensitive fragments, so a user can paste either the whole UA or a
// distinctive piece of it. An empty list never matches, which keeps every
// existing card relaying exactly as before.
func (u Upstream) RelayExempt(userAgent string) bool {
	return RelayExemptUserAgent(u.RelayExemptUserAgents, userAgent)
}

// RelayExemptUserAgent 是上面那条判断的独立形式：代理拿得到的是名单本身
// （构建时从配置里取出），不必持有整份上游配置。
func RelayExemptUserAgent(exempt []string, userAgent string) bool {
	candidate := strings.ToLower(strings.TrimSpace(userAgent))
	if candidate == "" {
		return false
	}
	for _, entry := range exempt {
		fragment := normalizeUserAgentFragment(entry)
		if fragment != "" && strings.Contains(candidate, fragment) {
			return true
		}
	}
	return false
}

// ListenAddr is the address the upstream's own reverse proxy listens on.
func (u Upstream) ListenAddr() string { return fmt.Sprintf(":%d", u.ListenPort) }

// Clone returns a deep copy so callers cannot mutate shared slices.
func (u Upstream) Clone() Upstream {
	copied := u
	if u.Enabled != nil {
		enabled := *u.Enabled
		copied.Enabled = &enabled
	}
	if u.IgnoreDirectPlayVerdict != nil {
		ignore := *u.IgnoreDirectPlayVerdict
		copied.IgnoreDirectPlayVerdict = &ignore
	}
	copied.StrmRoots = append([]string(nil), u.StrmRoots...)
	copied.PathMappings = append([]PathMapping(nil), u.PathMappings...)
	copied.RelayExemptUserAgents = append([]string(nil), u.RelayExemptUserAgents...)
	return copied
}

// Redirect holds the 302 behaviour shared by all upstreams.
type Redirect struct {
	TrustedProxyCIDRs []string     `yaml:"trusted_proxy_cidrs,omitempty" json:"trustedProxyCidrs"`
	Mode              RedirectMode `yaml:"mode" json:"mode"`
	// IntranetCIDRs 补充内置的内网判断。内置只认 RFC1918、回环、链路本地与
	// IPv6 ULA(fc00::/7)；而家里的设备拿到的常常是运营商下发的 IPv6 全局地址
	// （240e:: 这类 GUA），按地址类型它是公网，可它确实在局域网里 —— 于是
	// 「公网跳转」会错误地 302 给它、「内网跳转」又会漏掉它。命中所列网段
	// 即按内网处理，只影响跳转策略与日志里的内外网归属。
	IntranetCIDRs []string `yaml:"intranet_cidrs,omitempty" json:"intranetCidrs,omitempty"`
	// FollowUpstreamRedirects resolves intermediate 302 hops (common for 115
	// pick-code services) before answering the client, so players that do not
	// follow redirects still get a final URL.
	FollowUpstreamRedirects bool `yaml:"follow_upstream_redirects" json:"followUpstreamRedirects"`
	MaxFollowHops           int  `yaml:"max_follow_hops" json:"maxFollowHops"`
	// ForwardUserAgent keeps the player User-Agent when talking to the STRM
	// backend. Some cloud drives bind their signed URLs to the User-Agent.
	// It is a pointer so a hand-written config that omits the key keeps the
	// default (true) instead of silently turning the feature off.
	ForwardUserAgent                         *bool         `yaml:"forward_user_agent" json:"forwardUserAgent"`
	FallbackUserAgent                        string        `yaml:"fallback_user_agent" json:"fallbackUserAgent"`
	BlockClientUserAgent                     *bool         `yaml:"block_client_user_agent" json:"blockClientUserAgent"`
	BlockClientUserAgentEmby                 *bool         `yaml:"block_client_user_agent_emby" json:"blockClientUserAgentEmby"`
	BlockClientUserAgentAudiobookshelf       *bool         `yaml:"block_client_user_agent_audiobookshelf" json:"blockClientUserAgentAudiobookshelf"`
	BlockedUserAgents                        []string      `yaml:"blocked_user_agents,omitempty" json:"blockedUserAgents,omitempty"`
	BlockedUserAgentsEmby                    []string      `yaml:"blocked_user_agents_emby,omitempty" json:"blockedUserAgentsEmby,omitempty"`
	BlockClientUserAgentFnos                 *bool         `yaml:"block_client_user_agent_fnos,omitempty" json:"blockClientUserAgentFnos,omitempty"`
	BlockedUserAgentsFnos                    []string      `yaml:"blocked_user_agents_fnos,omitempty" json:"blockedUserAgentsFnos,omitempty"`
	BlockedUserAgentsFnosUpstreams           []string      `yaml:"blocked_user_agents_fnos_upstreams,omitempty" json:"blockedUserAgentsFnosUpstreams,omitempty"`
	BlockedUserAgentsAudiobookshelf          []string      `yaml:"blocked_user_agents_audiobookshelf,omitempty" json:"blockedUserAgentsAudiobookshelf,omitempty"`
	BlockedUserAgentsEmbyUpstreams           []string      `yaml:"blocked_user_agents_emby_upstreams,omitempty" json:"blockedUserAgentsEmbyUpstreams,omitempty"`
	BlockedUserAgentsAudiobookshelfUpstreams []string      `yaml:"blocked_user_agents_audiobookshelf_upstreams,omitempty" json:"blockedUserAgentsAudiobookshelfUpstreams,omitempty"`
	ProbeTimeout                             time.Duration `yaml:"probe_timeout" json:"probeTimeout"`
	StreamTimeout                            time.Duration `yaml:"stream_timeout" json:"streamTimeout"`
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
	return r.IsBlockedClientUserAgentFor("", userAgent)
}

// IsBlockedClientUserAgentFor applies provider-specific keyword lists. A
// legacy shared list remains active for configurations created before the
// provider split was introduced.
func (r Redirect) IsBlockedClientUserAgentFor(provider UpstreamType, userAgent string) bool {
	return r.IsBlockedClientUserAgentForUpstream(provider, "", userAgent)
}

// IsBlockedClientUserAgentForUpstream applies provider-specific rules to one
// configured upstream. New configurations only block candidates selected in
// the settings page; the legacy shared switch/list remains compatible.
func (r Redirect) IsBlockedClientUserAgentForUpstream(provider UpstreamType, upstreamName, userAgent string) bool {
	providerEnabled, scoped := r.providerBlockSettings(provider)
	if scoped {
		if !providerEnabled || !containsString(providerBlockUpstreams(r, provider), upstreamName) {
			return false
		}
	} else if !r.ShouldBlockClientUserAgent() {
		return false
	}
	userAgent = strings.ToLower(strings.TrimSpace(userAgent))
	if userAgent == "" {
		return false
	}
	blockedLists := [][]string{r.BlockedUserAgents}
	switch provider {
	case UpstreamEmby:
		blockedLists = append(blockedLists, r.BlockedUserAgentsEmby)
	case UpstreamFnos:
		// 飞牛影视有独立名单；旧配置没有飞牛专属字段时回落到 Emby 共用名单。
		if r.BlockClientUserAgentFnos != nil || len(r.BlockedUserAgentsFnos) > 0 {
			blockedLists = append(blockedLists, r.BlockedUserAgentsFnos)
		} else {
			blockedLists = append(blockedLists, r.BlockedUserAgentsEmby)
		}
	case UpstreamAudiobookshelf:
		blockedLists = append(blockedLists, r.BlockedUserAgentsAudiobookshelf)
	}
	for _, blockedList := range blockedLists {
		for _, blocked := range blockedList {
			blocked = normalizeUserAgentFragment(blocked)
			if blocked != "" && strings.Contains(userAgent, blocked) {
				return true
			}
		}
	}
	return false
}

func (r Redirect) providerBlockSettings(provider UpstreamType) (enabled, scoped bool) {
	switch provider {
	case UpstreamEmby:
		if r.BlockClientUserAgentEmby != nil {
			return *r.BlockClientUserAgentEmby, true
		}
	case UpstreamFnos:
		// 飞牛影视独立开关；旧配置没有该字段时回落到 Emby 共用开关。
		if r.BlockClientUserAgentFnos != nil {
			return *r.BlockClientUserAgentFnos, true
		}
		if r.BlockClientUserAgentEmby != nil {
			return *r.BlockClientUserAgentEmby, true
		}
	case UpstreamAudiobookshelf:
		if r.BlockClientUserAgentAudiobookshelf != nil {
			return *r.BlockClientUserAgentAudiobookshelf, true
		}
	}
	return false, false
}

func providerBlockUpstreams(r Redirect, provider UpstreamType) []string {
	switch provider {
	case UpstreamEmby:
		return r.BlockedUserAgentsEmbyUpstreams
	case UpstreamFnos:
		// 飞牛影视独立勾选；旧配置没有该字段时回落到 Emby 共用勾选。
		if r.BlockClientUserAgentFnos != nil || len(r.BlockedUserAgentsFnosUpstreams) > 0 {
			return r.BlockedUserAgentsFnosUpstreams
		}
		return r.BlockedUserAgentsEmbyUpstreams
	case UpstreamAudiobookshelf:
		return r.BlockedUserAgentsAudiobookshelfUpstreams
	}
	return nil
}

func containsString(values []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), target) {
			return true
		}
	}
	return false
}

func normalizeUserAgentFragment(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeUserAgentList(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, strings.TrimSpace(value))
		}
	}
	return result
}

func normalizeStringList(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
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
	// Username is the admin account name. 界面在设置页的「登录账号」卡片里回显它；
	// 登录页不回显（那是个免鉴权页面，不给扫端口的人任何线索）。
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
	// envKept 记录环境变量覆盖前的原值，Save 时用它们把「启动期覆盖」挡在
	// 磁盘之外：详见 applyEnvOverrides 的说明。
	envKept envKeptValues `yaml:"-"`
}

// envKeptValues 保存被环境变量覆盖掉的、原本写在配置文件里的值。
type envKeptValues struct {
	hasListen  bool
	listen     string
	hasToken   bool
	adminToken string
}

// Path returns the file the config was loaded from.
func (c *Config) Path() string { return c.path }

// SetPath records where the config should be saved.
func (c *Config) SetPath(path string) { c.path = path }

// Clone returns a deep copy of the configuration so an admin request can edit a
// candidate config without touching the one currently serving traffic.
func (c *Config) Clone() *Config {
	copied := *c
	copied.Redirect.TrustedProxyCIDRs = append([]string(nil), c.Redirect.TrustedProxyCIDRs...)
	copied.Redirect.IntranetCIDRs = append([]string(nil), c.Redirect.IntranetCIDRs...)
	copied.Redirect.ForwardUserAgent = clonePointer(c.Redirect.ForwardUserAgent)
	copied.Redirect.BlockClientUserAgent = clonePointer(c.Redirect.BlockClientUserAgent)
	copied.Redirect.BlockClientUserAgentEmby = clonePointer(c.Redirect.BlockClientUserAgentEmby)
	copied.Redirect.BlockClientUserAgentAudiobookshelf = clonePointer(c.Redirect.BlockClientUserAgentAudiobookshelf)
	copied.Redirect.BlockedUserAgents = append([]string(nil), c.Redirect.BlockedUserAgents...)
	copied.Redirect.BlockedUserAgentsEmby = append([]string(nil), c.Redirect.BlockedUserAgentsEmby...)
	copied.Redirect.BlockClientUserAgentFnos = clonePointer(c.Redirect.BlockClientUserAgentFnos)
	copied.Redirect.BlockedUserAgentsFnos = append([]string(nil), c.Redirect.BlockedUserAgentsFnos...)
	copied.Redirect.BlockedUserAgentsFnosUpstreams = append([]string(nil), c.Redirect.BlockedUserAgentsFnosUpstreams...)
	copied.Redirect.BlockedUserAgentsAudiobookshelf = append([]string(nil), c.Redirect.BlockedUserAgentsAudiobookshelf...)
	copied.Redirect.BlockedUserAgentsEmbyUpstreams = append([]string(nil), c.Redirect.BlockedUserAgentsEmbyUpstreams...)
	copied.Redirect.BlockedUserAgentsAudiobookshelfUpstreams = append([]string(nil), c.Redirect.BlockedUserAgentsAudiobookshelfUpstreams...)
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
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, false, err
	}
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
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, err
	}
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
// 两处已废弃字段在这里被清掉（Prefix 的路径前缀、IgnoreDirectPlayVerdict 的
// 忽略判定开关）：Load 用的是严格解析，字段声明得留着才能把老配置读进来，
// 但值已经不再被采纳，清空后下一次保存就不再写出它们。
//
// 另外老配置没有 listen_port，这里按管理端口往上顺次分配一个空闲端口。
func (c *Config) migrate() bool {
	changed := false
	if c.ShouldUseLegacyUserAgentScope() {
		for _, upstream := range c.Upstreams {
			switch upstream.Type {
			case UpstreamEmby, UpstreamFnos:
				c.Redirect.BlockedUserAgentsEmbyUpstreams = appendUniqueString(c.Redirect.BlockedUserAgentsEmbyUpstreams, upstream.Name)
			case UpstreamAudiobookshelf:
				c.Redirect.BlockedUserAgentsAudiobookshelfUpstreams = appendUniqueString(c.Redirect.BlockedUserAgentsAudiobookshelfUpstreams, upstream.Name)
			}
		}
		if c.Redirect.BlockClientUserAgentEmby == nil {
			c.Redirect.BlockClientUserAgentEmby = Bool(true)
			changed = true
		}
		if c.Redirect.BlockClientUserAgentAudiobookshelf == nil {
			c.Redirect.BlockClientUserAgentAudiobookshelf = Bool(true)
			changed = true
		}
	}
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
		if upstream.IgnoreDirectPlayVerdict != nil {
			upstream.IgnoreDirectPlayVerdict = nil
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

func (c *Config) ShouldUseLegacyUserAgentScope() bool {
	return c.Redirect.ShouldBlockClientUserAgent() && c.Redirect.BlockClientUserAgentEmby == nil && c.Redirect.BlockClientUserAgentAudiobookshelf == nil
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if strings.EqualFold(strings.TrimSpace(existing), strings.TrimSpace(value)) {
			return values
		}
	}
	return append(values, value)
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
	if parsed.Redirect.BlockClientUserAgentEmby != nil {
		base.Redirect.BlockClientUserAgentEmby = parsed.Redirect.BlockClientUserAgentEmby
	}
	if parsed.Redirect.BlockClientUserAgentAudiobookshelf != nil {
		base.Redirect.BlockClientUserAgentAudiobookshelf = parsed.Redirect.BlockClientUserAgentAudiobookshelf
	}
	if parsed.Redirect.BlockedUserAgents != nil {
		base.Redirect.BlockedUserAgents = append([]string(nil), parsed.Redirect.BlockedUserAgents...)
	}
	if parsed.Redirect.BlockedUserAgentsEmby != nil {
		base.Redirect.BlockedUserAgentsEmby = append([]string(nil), parsed.Redirect.BlockedUserAgentsEmby...)
	}
	if parsed.Redirect.BlockClientUserAgentFnos != nil {
		base.Redirect.BlockClientUserAgentFnos = parsed.Redirect.BlockClientUserAgentFnos
	}
	if parsed.Redirect.BlockedUserAgentsFnos != nil {
		base.Redirect.BlockedUserAgentsFnos = append([]string(nil), parsed.Redirect.BlockedUserAgentsFnos...)
	}
	if parsed.Redirect.BlockedUserAgentsFnosUpstreams != nil {
		base.Redirect.BlockedUserAgentsFnosUpstreams = append([]string(nil), parsed.Redirect.BlockedUserAgentsFnosUpstreams...)
	}
	if parsed.Redirect.BlockedUserAgentsAudiobookshelf != nil {
		base.Redirect.BlockedUserAgentsAudiobookshelf = append([]string(nil), parsed.Redirect.BlockedUserAgentsAudiobookshelf...)
	}
	if parsed.Redirect.BlockedUserAgentsEmbyUpstreams != nil {
		base.Redirect.BlockedUserAgentsEmbyUpstreams = append([]string(nil), parsed.Redirect.BlockedUserAgentsEmbyUpstreams...)
	}
	if parsed.Redirect.BlockedUserAgentsAudiobookshelfUpstreams != nil {
		base.Redirect.BlockedUserAgentsAudiobookshelfUpstreams = append([]string(nil), parsed.Redirect.BlockedUserAgentsAudiobookshelfUpstreams...)
	}
	if parsed.Redirect.AllowPublicTargets != nil {
		base.Redirect.AllowPublicTargets = parsed.Redirect.AllowPublicTargets
	}
	if parsed.Redirect.TrustedProxyCIDRs != nil {
		base.Redirect.TrustedProxyCIDRs = append([]string(nil), parsed.Redirect.TrustedProxyCIDRs...)
	}
	if parsed.Redirect.IntranetCIDRs != nil {
		base.Redirect.IntranetCIDRs = append([]string(nil), parsed.Redirect.IntranetCIDRs...)
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
//
// 这些覆盖只作用于本次启动：Save 会把配置文件里原本的值写回去，不让环境变量
// 顺带固化到磁盘上。否则删掉 AETHERLINK_PORT 之后监听端口会被永久改掉（compose
// 里映射 5151 的写法就再也进不去管理页），删掉 AETHERLINK_ADMIN_TOKEN 之后
// 那个应急令牌还会一直留在文件里。
func applyEnvOverrides(cfg *Config) error {
	// AETHERLINK_PORT 是给容器用的简写：只写端口号，compose 里同一个变量既能改
	// 容器内的监听端口、又能改端口映射。不设置时完全不介入，配置文件里的
	// listen 照旧生效。AETHERLINK_LISTEN 接受完整地址（`:8080`、
	// `127.0.0.1:8080`），两者同时存在以它为准。
	if value := strings.TrimSpace(os.Getenv("AETHERLINK_PORT")); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("AETHERLINK_PORT %q 不是 1-65535 之间的端口号", value)
		}
		cfg.rememberFileListen()
		cfg.Server.Listen = ":" + value
	}
	if value := os.Getenv("AETHERLINK_LISTEN"); value != "" {
		cfg.rememberFileListen()
		cfg.Server.Listen = value
	}
	if value := os.Getenv("AETHERLINK_LOG_LEVEL"); value != "" {
		cfg.Server.LogLevel = value
	}
	if value := os.Getenv("AETHERLINK_ADMIN_TOKEN"); value != "" {
		if !cfg.envKept.hasToken {
			cfg.envKept.hasToken = true
			cfg.envKept.adminToken = cfg.Server.AdminToken
		}
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
	return nil
}

// rememberFileListen 在环境变量改写监听地址之前，记下配置文件里的原值。
func (c *Config) rememberFileListen() {
	if c.envKept.hasListen {
		return
	}
	c.envKept.hasListen = true
	c.envKept.listen = c.Server.Listen
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
	case RedirectAlways, RedirectPublic, RedirectPrivate, RedirectNever:
	default:
		return fmt.Errorf("redirect.mode %q 必须是 always、public、private 或 never", c.Redirect.Mode)
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
	c.Redirect.TrustedProxyCIDRs = normalizeStringList(c.Redirect.TrustedProxyCIDRs)
	for _, value := range c.Redirect.TrustedProxyCIDRs {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Bits() == 0 {
			return fmt.Errorf("可信代理 %q 必须为明确的 IP/CIDR 网段，不能信任全部地址", value)
		}
	}
	// 内网网段：命中的客户端一律按内网处理，所以只拦「不是网段」与「覆盖全部
	// 地址」两种。后者会让内外网判断整体失效（效果等同于把跳转模式改成始终
	// 跳转或始终中继），真有那种需求应当直接改模式。
	c.Redirect.IntranetCIDRs = normalizeStringList(c.Redirect.IntranetCIDRs)
	for _, value := range c.Redirect.IntranetCIDRs {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return fmt.Errorf("内网网段 %q 不是合法的 IP/CIDR 网段", value)
		}
		if prefix.Bits() == 0 {
			return fmt.Errorf("内网网段 %q 覆盖全部地址，会让内外网判断失效：请改用跳转模式表达", value)
		}
	}
	// 飞牛影视屏蔽 UA 是从 Emby 共用配置拆出来的：老配置没有飞牛专属字段，
	// 这里照搬 Emby 的开关、名单与勾选，行为与拆分前完全一致；首次保存后
	// 飞牛字段即显式落盘，之后两边各自独立。
	if c.Redirect.BlockClientUserAgentFnos == nil {
		c.Redirect.BlockClientUserAgentFnos = Bool(c.Redirect.BlockClientUserAgentEmby != nil && *c.Redirect.BlockClientUserAgentEmby)
		c.Redirect.BlockedUserAgentsFnos = append([]string(nil), c.Redirect.BlockedUserAgentsEmby...)
		c.Redirect.BlockedUserAgentsFnosUpstreams = append([]string(nil), c.Redirect.BlockedUserAgentsEmbyUpstreams...)
	}
	c.Redirect.BlockedUserAgentsEmby = normalizeUserAgentList(c.Redirect.BlockedUserAgentsEmby)
	c.Redirect.BlockedUserAgentsFnos = normalizeUserAgentList(c.Redirect.BlockedUserAgentsFnos)
	c.Redirect.BlockedUserAgentsAudiobookshelf = normalizeUserAgentList(c.Redirect.BlockedUserAgentsAudiobookshelf)
	c.Redirect.BlockedUserAgentsEmbyUpstreams = normalizeStringList(c.Redirect.BlockedUserAgentsEmbyUpstreams)
	c.Redirect.BlockedUserAgentsFnosUpstreams = normalizeStringList(c.Redirect.BlockedUserAgentsFnosUpstreams)
	c.Redirect.BlockedUserAgentsAudiobookshelfUpstreams = normalizeStringList(c.Redirect.BlockedUserAgentsAudiobookshelfUpstreams)
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
	case UpstreamAudiobookshelf, UpstreamEmby, UpstreamFnos:
	case "":
		return fmt.Errorf("上游 %s 缺少类型，必须是 audiobookshelf、emby 或 fnos", u.Name)
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
	u.Username = strings.TrimSpace(u.Username)
	// 密码原样保留：它是口令，首尾空格也可能真的属于密码，这里不做裁剪。
	// 只有一半的凭据既登不上去，又会让界面误以为「已配置」，所以直接拒掉。
	if (u.Username == "") != (u.Password == "") {
		return fmt.Errorf("上游 %s 的登录账号与密码必须同时填写", u.Name)
	}
	if u.RedirectMode == "" {
		u.RedirectMode = RedirectAlways
	}
	switch u.RedirectMode {
	case RedirectAlways, RedirectPublic, RedirectPrivate, RedirectNever:
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
	// 「不中继的客户端」与屏蔽 UA 同一套写法：去空、去重、大小写统一。
	u.RelayExemptUserAgents = normalizeUserAgentList(u.RelayExemptUserAgents)
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
	// 环境变量覆盖过的监听地址与应急令牌只作用于本次启动，写盘时换回文件里
	// 原本的值，避免「临时加了个变量」变成永久改动。
	snapshot := *c
	if c.envKept.hasListen {
		snapshot.Server.Listen = c.envKept.listen
	}
	if c.envKept.hasToken {
		snapshot.Server.AdminToken = c.envKept.adminToken
	}
	data, err := yaml.Marshal(&snapshot)
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
