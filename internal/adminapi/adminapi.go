// Package adminapi 提供 Vue 管理界面使用的全部管理接口。
//
// AetherLink 的配置完全由这里读写：docker compose 只需要挂载一个 /config 卷，
// 管理口令、Audiobookshelf / Emby 上游地址与 API 密钥、302 跳转策略等都在
// 网页上填写并即时生效。因此除了健康检查、首次初始化和登录，其余路由都必须
// 携带会话令牌——否则任何能访问端口的人都能读到上游密钥。
package adminapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aetherlink/aetherlink/internal/auth"
	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/logx"
	"github.com/aetherlink/aetherlink/internal/pathmap"
	"github.com/aetherlink/aetherlink/internal/resolver"
	"github.com/aetherlink/aetherlink/internal/runtime"
	"github.com/aetherlink/aetherlink/internal/strm"
	"github.com/aetherlink/aetherlink/internal/upstream"
)

// BasePath 是所有管理接口的挂载前缀。
const BasePath = "/aetherlink/api"

// Version 由构建时注入，状态接口会展示它。
var Version = "dev"

// maxBodyBytes 限制管理接口的请求体大小，路径映射再多也用不了 1 MiB。
const maxBodyBytes = 1 << 20

// API 提供管理接口。
type API struct {
	rt       *runtime.Runtime
	sessions *auth.Store
	started  time.Time
}

// New 构建管理接口。
func New(rt *runtime.Runtime, sessions *auth.Store) *API {
	return &API{rt: rt, sessions: sessions, started: time.Now()}
}

// Handler 返回挂在 BasePath 下的路由。
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// 无需鉴权：容器健康检查、登录前的界面自举信息与登录本身。
	mux.HandleFunc("GET "+BasePath+"/health", a.handleHealth)
	mux.HandleFunc("GET "+BasePath+"/bootstrap", a.handleBootstrap)
	mux.HandleFunc("POST "+BasePath+"/login", a.handleLogin)
	mux.HandleFunc("POST "+BasePath+"/logout", a.handleLogout)

	mux.HandleFunc("GET "+BasePath+"/status", a.protected(a.handleStatus))
	mux.HandleFunc("GET "+BasePath+"/config", a.protected(a.handleGetConfig))
	mux.HandleFunc("PUT "+BasePath+"/settings", a.protected(a.handlePutSettings))
	mux.HandleFunc("POST "+BasePath+"/account", a.protected(a.handleUpdateAccount))

	mux.HandleFunc("GET "+BasePath+"/upstreams", a.protected(a.handleUpstreams))
	mux.HandleFunc("POST "+BasePath+"/upstreams", a.protected(a.handleCreateUpstream))
	mux.HandleFunc("POST "+BasePath+"/upstreams/test", a.protected(a.handleTestUpstream))
	mux.HandleFunc("PUT "+BasePath+"/upstreams/{name}", a.protected(a.handleUpdateUpstream))
	mux.HandleFunc("DELETE "+BasePath+"/upstreams/{name}", a.protected(a.handleDeleteUpstream))

	mux.HandleFunc("GET "+BasePath+"/upstreams/{name}/ping", a.protected(a.handlePing))
	mux.HandleFunc("GET "+BasePath+"/upstreams/{name}/libraries", a.protected(a.handleLibraries))
	mux.HandleFunc("GET "+BasePath+"/upstreams/{name}/items", a.protected(a.handleItems))
	mux.HandleFunc("GET "+BasePath+"/upstreams/{name}/items/{itemId}", a.protected(a.handleItemFiles))
	mux.HandleFunc("GET "+BasePath+"/upstreams/{name}/resolve", a.protected(a.handleResolve))

	mux.HandleFunc("POST "+BasePath+"/strm/parse", a.protected(a.handleParseStrm))
	mux.HandleFunc("GET "+BasePath+"/stats", a.protected(a.handleStats))
	mux.HandleFunc("GET "+BasePath+"/logs", a.protected(a.handleLogs))
	mux.HandleFunc("POST "+BasePath+"/cache/purge", a.protected(a.handlePurgeCache))
	return mux
}

// protected 要求请求带上有效的会话令牌，或者配置里的应急令牌。
func (a *API) protected(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if !a.authorized(request) {
			writer.Header().Set("WWW-Authenticate", "Bearer realm=aetherlink")
			writeJSON(writer, http.StatusUnauthorized, map[string]any{
				"error": "会话无效或已过期，请重新登录",
				"code":  "unauthorized",
			})
			return
		}
		next(writer, request)
	}
}

func (a *API) authorized(request *http.Request) bool {
	token := bearerToken(request)
	if token == "" {
		return false
	}
	if a.sessions.Valid(token) {
		return true
	}
	// 应急令牌只在忘记口令时通过 AETHERLINK_ADMIN_TOKEN 注入，平时为空。
	breakGlass := strings.TrimSpace(a.rt.Config().Server.AdminToken)
	if breakGlass == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(breakGlass)) == 1
}

func bearerToken(request *http.Request) string {
	if header := request.Header.Get("Authorization"); header != "" {
		return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	}
	if header := request.Header.Get("X-AetherLink-Token"); header != "" {
		return strings.TrimSpace(header)
	}
	return strings.TrimSpace(request.URL.Query().Get("token"))
}

func (a *API) handleHealth(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "version": Version})
}

// handleBootstrap 给登录页提供最少的自举信息：版本号、是否仍是默认凭据。
// 不需要鉴权，因此绝不能在这里泄露上游地址、密钥或配置文件路径。
func (a *API) handleBootstrap(writer http.ResponseWriter, request *http.Request) {
	cfg := a.rt.Config()
	payload := map[string]any{
		"version":            Version,
		"defaultCredentials": cfg.Auth.DefaultCredentials,
		"minPasswordLength":  auth.MinPasswordLength,
	}
	if cfg.Auth.DefaultCredentials {
		// 仅在还没改过账号时回显内置凭据，用来预填登录框。
		// 改过之后就不再提，免得给暴力破解者多一条线索。
		payload["defaultUsername"] = auth.DefaultUsername
		payload["defaultPassword"] = auth.DefaultPassword
	}
	writeJSON(writer, http.StatusOK, payload)
}

type loginPayload struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type accountPayload struct {
	CurrentPassword string `json:"currentPassword"`
	Username        string `json:"username"`
	NewPassword     string `json:"newPassword"`
}

func (a *API) handleLogin(writer http.ResponseWriter, request *http.Request) {
	var payload loginPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}
	cfg := a.rt.Config()
	if err := auth.VerifyLogin(cfg.Auth, payload.Username, payload.Password); err != nil {
		// 统一返回 401 且不区分原因，避免暴露用户名是否存在这类细节。
		logx.Warnf("[adminapi] 登录失败：%v", err)
		writeError(writer, http.StatusUnauthorized, "账号或密码不正确")
		return
	}
	a.issueSession(writer)
}

func (a *API) issueSession(writer http.ResponseWriter) {
	token, expires, err := a.sessions.Issue()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "签发会话失败: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"token": token, "expiresAt": expires})
}

func (a *API) handleLogout(writer http.ResponseWriter, request *http.Request) {
	a.sessions.Revoke(bearerToken(request))
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

// handleUpdateAccount 修改用户名与/或密码。改完注销所有会话，包括调用方自己。
// 只想改用户名时把新密码留空即可，但当前密码始终要验，避免会话被盗后直接改账号。
func (a *API) handleUpdateAccount(writer http.ResponseWriter, request *http.Request) {
	var payload accountPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}
	cfg := a.rt.Config()
	if err := auth.Verify(cfg.Auth, payload.CurrentPassword); err != nil {
		writeError(writer, http.StatusUnauthorized, "当前密码不正确")
		return
	}

	username := auth.NormalizeUsername(payload.Username)
	if username == "" {
		username = cfg.Auth.Username
	}
	password := payload.NewPassword
	if strings.TrimSpace(password) == "" {
		// 只改用户名：沿用当前密码需要重新派生，因为盐与哈希是绑在一起的。
		password = payload.CurrentPassword
	}

	derived, err := auth.Derive(username, password)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.rt.Apply(func(draft *config.Config) error {
		draft.Auth = derived
		return nil
	}); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	a.sessions.RevokeAll()
	logx.Infof("[adminapi] 管理账号已更新为 %s，所有会话已注销", username)
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "reloginRequired": true})
}

type statusResponse struct {
	Version         string    `json:"version"`
	StartedAt       time.Time `json:"startedAt"`
	UptimeSeconds   int64     `json:"uptimeSeconds"`
	Listen          string    `json:"listen"`
	RedirectMode    string    `json:"redirectMode"`
	FollowHops      bool      `json:"followUpstreamRedirects"`
	CacheEntries    int       `json:"cacheEntries"`
	CacheTTL        string    `json:"cacheTtl"`
	UpstreamCount   int       `json:"upstreamCount"`
	EnabledCount    int       `json:"enabledUpstreamCount"`
	Sessions        int       `json:"sessions"`
	Username        string    `json:"username"`
	DefaultCreds    bool      `json:"defaultCredentials"`
	ConfigPath      string    `json:"configPath"`
	RestartRequired bool      `json:"restartRequired"`
	BootListen      string    `json:"bootListen"`
}

func (a *API) handleStatus(writer http.ResponseWriter, request *http.Request) {
	cfg := a.rt.Config()
	enabled := 0
	for _, up := range cfg.Upstreams {
		if up.IsEnabled() {
			enabled++
		}
	}
	writeJSON(writer, http.StatusOK, statusResponse{
		Version:         Version,
		StartedAt:       a.started,
		UptimeSeconds:   int64(time.Since(a.started).Seconds()),
		Listen:          cfg.Server.Listen,
		RedirectMode:    string(cfg.Redirect.Mode),
		FollowHops:      cfg.Redirect.FollowUpstreamRedirects,
		CacheEntries:    a.rt.Resolver().CacheSize(),
		CacheTTL:        cfg.Cache.TTL.String(),
		UpstreamCount:   len(cfg.Upstreams),
		EnabledCount:    enabled,
		Sessions:        a.sessions.Count(),
		Username:        cfg.Auth.Username,
		DefaultCreds:    cfg.Auth.DefaultCredentials,
		ConfigPath:      a.rt.ConfigPath(),
		RestartRequired: a.rt.RestartRequired(),
		BootListen:      a.rt.BootListen(),
	})
}

// settingsPayload 是 GET /config 与 PUT /settings 共用的形状，时长用字符串
// 表示（例如 "5m"），这样网页上填写和展示都直观。
type settingsPayload struct {
	LogLevel  string           `json:"logLevel"`
	LogBuffer int              `json:"logBuffer"`
	Redirect  redirectSettings `json:"redirect"`
	Cache     cacheSettings    `json:"cache"`
}

type redirectSettings struct {
	Mode                    string `json:"mode"`
	FollowUpstreamRedirects bool   `json:"followUpstreamRedirects"`
	MaxFollowHops           int    `json:"maxFollowHops"`
	ForwardUserAgent        bool   `json:"forwardUserAgent"`
	FallbackUserAgent       string `json:"fallbackUserAgent"`
	ProbeTimeout            string `json:"probeTimeout"`
	StreamTimeout           string `json:"streamTimeout"`
	AllowPublicTargets      bool   `json:"allowPublicTargets"`
}

type cacheSettings struct {
	TTL     string `json:"ttl"`
	MaxSize int    `json:"maxSize"`
}

func settingsFromConfig(cfg *config.Config) settingsPayload {
	return settingsPayload{
		LogLevel:  cfg.Server.LogLevel,
		LogBuffer: cfg.Server.LogBuffer,
		Redirect: redirectSettings{
			Mode:                    string(cfg.Redirect.Mode),
			FollowUpstreamRedirects: cfg.Redirect.FollowUpstreamRedirects,
			MaxFollowHops:           cfg.Redirect.MaxFollowHops,
			ForwardUserAgent:        cfg.Redirect.ShouldForwardUserAgent(),
			FallbackUserAgent:       cfg.Redirect.FallbackUserAgent,
			ProbeTimeout:            cfg.Redirect.ProbeTimeout.String(),
			StreamTimeout:           cfg.Redirect.StreamTimeout.String(),
			AllowPublicTargets:      cfg.Redirect.PublicTargetsAllowed(),
		},
		Cache: cacheSettings{TTL: cfg.Cache.TTL.String(), MaxSize: cfg.Cache.MaxSize},
	}
}

func (a *API) handleGetConfig(writer http.ResponseWriter, request *http.Request) {
	cfg := a.rt.Config()
	writeJSON(writer, http.StatusOK, map[string]any{
		"server": map[string]any{
			"listen":            cfg.Server.Listen,
			"configPath":        a.rt.ConfigPath(),
			"restartRequired":   a.rt.RestartRequired(),
			"breakGlassEnabled": strings.TrimSpace(cfg.Server.AdminToken) != "",
		},
		"account": map[string]any{
			"username":           cfg.Auth.Username,
			"defaultCredentials": cfg.Auth.DefaultCredentials,
			"minPasswordLength":  auth.MinPasswordLength,
		},
		"settings":  settingsFromConfig(cfg),
		"upstreams": a.describeUpstreams(cfg.Upstreams),
	})
}

func (a *API) handlePutSettings(writer http.ResponseWriter, request *http.Request) {
	current := a.rt.Config()
	// 以当前值为默认，缺省字段不会被意外清零。
	payload := settingsFromConfig(current)
	if !decodeJSON(writer, request, &payload) {
		return
	}
	probeTimeout, err := parseDuration(payload.Redirect.ProbeTimeout, current.Redirect.ProbeTimeout)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "redirect.probeTimeout "+err.Error())
		return
	}
	streamTimeout, err := parseDuration(payload.Redirect.StreamTimeout, current.Redirect.StreamTimeout)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "redirect.streamTimeout "+err.Error())
		return
	}
	cacheTTL, err := parseDuration(payload.Cache.TTL, current.Cache.TTL)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "cache.ttl "+err.Error())
		return
	}

	if err := a.rt.Apply(func(draft *config.Config) error {
		if strings.TrimSpace(payload.LogLevel) != "" {
			draft.Server.LogLevel = strings.TrimSpace(payload.LogLevel)
		}
		if payload.LogBuffer > 0 {
			draft.Server.LogBuffer = payload.LogBuffer
		}
		draft.Redirect.Mode = config.RedirectMode(strings.TrimSpace(payload.Redirect.Mode))
		draft.Redirect.FollowUpstreamRedirects = payload.Redirect.FollowUpstreamRedirects
		draft.Redirect.MaxFollowHops = payload.Redirect.MaxFollowHops
		draft.Redirect.ForwardUserAgent = &payload.Redirect.ForwardUserAgent
		draft.Redirect.AllowPublicTargets = &payload.Redirect.AllowPublicTargets
		draft.Redirect.FallbackUserAgent = payload.Redirect.FallbackUserAgent
		draft.Redirect.ProbeTimeout = probeTimeout
		draft.Redirect.StreamTimeout = streamTimeout
		draft.Cache.TTL = cacheTTL
		draft.Cache.MaxSize = payload.Cache.MaxSize
		return nil
	}); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "settings": settingsFromConfig(a.rt.Config())})
}

type upstreamSummary struct {
	Name         string               `json:"name"`
	Type         string               `json:"type"`
	BaseURL      string               `json:"baseUrl"`
	Prefix       string               `json:"prefix"`
	Enabled      bool                 `json:"enabled"`
	HasAPIKey    bool                 `json:"hasApiKey"`
	Insecure     bool                 `json:"insecureSkipVerify"`
	StrmRoots    []string             `json:"strmRoots"`
	PathMappings []config.PathMapping `json:"pathMappings"`
	// Active 表示该上游当前是否已在反向代理中挂载。
	Active bool `json:"active"`
}

func (a *API) describeUpstreams(upstreams []config.Upstream) []upstreamSummary {
	summaries := make([]upstreamSummary, 0, len(upstreams))
	for _, up := range upstreams {
		summary := upstreamSummary{
			Name:         up.Name,
			Type:         string(up.Type),
			BaseURL:      up.BaseURL,
			Prefix:       up.Prefix,
			Enabled:      up.IsEnabled(),
			HasAPIKey:    strings.TrimSpace(up.APIKey) != "",
			Insecure:     up.Insecure,
			StrmRoots:    up.StrmRoots,
			PathMappings: up.PathMappings,
			Active:       a.rt.ProviderByName(up.Name) != nil,
		}
		if summary.StrmRoots == nil {
			summary.StrmRoots = []string{}
		}
		if summary.PathMappings == nil {
			summary.PathMappings = []config.PathMapping{}
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

func (a *API) handleUpstreams(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"upstreams": a.describeUpstreams(a.rt.Config().Upstreams)})
}

// upstreamPayload 是新增/修改上游的请求体。APIKey 用指针：省略表示保留原有
// 密钥，这样界面上无需回显密钥也能编辑其他字段。
type upstreamPayload struct {
	Name         string               `json:"name"`
	Type         string               `json:"type"`
	BaseURL      string               `json:"baseUrl"`
	APIKey       *string              `json:"apiKey"`
	Enabled      *bool                `json:"enabled"`
	Prefix       string               `json:"prefix"`
	Insecure     bool                 `json:"insecureSkipVerify"`
	StrmRoots    []string             `json:"strmRoots"`
	PathMappings []config.PathMapping `json:"pathMappings"`
}

// toConfig 把请求体转成配置项，existing 非空时继承其密钥。
func (p upstreamPayload) toConfig(existing *config.Upstream) config.Upstream {
	result := config.Upstream{
		Name:         strings.TrimSpace(p.Name),
		Type:         config.UpstreamType(strings.TrimSpace(p.Type)),
		BaseURL:      strings.TrimSpace(p.BaseURL),
		Enabled:      p.Enabled,
		Prefix:       p.Prefix,
		Insecure:     p.Insecure,
		StrmRoots:    p.StrmRoots,
		PathMappings: p.PathMappings,
	}
	switch {
	case p.APIKey != nil:
		result.APIKey = strings.TrimSpace(*p.APIKey)
	case existing != nil:
		result.APIKey = existing.APIKey
	}
	if result.Enabled == nil && existing != nil {
		result.Enabled = existing.Enabled
	}
	return result
}

func (a *API) handleCreateUpstream(writer http.ResponseWriter, request *http.Request) {
	var payload upstreamPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}
	entry := payload.toConfig(nil)
	if err := a.rt.Apply(func(draft *config.Config) error {
		if draft.UpstreamByName(entry.Name) != nil {
			return fmt.Errorf("上游 %q 已存在", entry.Name)
		}
		draft.Upstreams = append(draft.Upstreams, entry)
		return nil
	}); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	logx.Infof("[adminapi] 已新增上游 %s (%s)", entry.Name, entry.Type)
	writeJSON(writer, http.StatusCreated, map[string]any{"ok": true, "upstreams": a.describeUpstreams(a.rt.Config().Upstreams)})
}

func (a *API) handleUpdateUpstream(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	existing := a.rt.Config().UpstreamByName(name)
	if existing == nil {
		writeError(writer, http.StatusNotFound, "上游 "+name+" 不存在")
		return
	}
	payload := upstreamPayload{
		Name:         existing.Name,
		Type:         string(existing.Type),
		BaseURL:      existing.BaseURL,
		Prefix:       existing.Prefix,
		Insecure:     existing.Insecure,
		StrmRoots:    existing.StrmRoots,
		PathMappings: existing.PathMappings,
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	updated := payload.toConfig(existing)
	if err := a.rt.Apply(func(draft *config.Config) error {
		target := draft.UpstreamByName(name)
		if target == nil {
			return errors.New("上游 " + name + " 不存在")
		}
		if updated.Name != name && draft.UpstreamByName(updated.Name) != nil {
			return fmt.Errorf("上游 %q 已存在", updated.Name)
		}
		*target = updated
		return nil
	}); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	// 改动上游意味着解析结果可能失效（例如换了路径映射），旧缓存必须丢掉。
	a.rt.Resolver().PurgeCache()
	logx.Infof("[adminapi] 已更新上游 %s", name)
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "upstreams": a.describeUpstreams(a.rt.Config().Upstreams)})
}

func (a *API) handleDeleteUpstream(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	if err := a.rt.Apply(func(draft *config.Config) error {
		filtered := make([]config.Upstream, 0, len(draft.Upstreams))
		found := false
		for _, up := range draft.Upstreams {
			if up.Name == name {
				found = true
				continue
			}
			filtered = append(filtered, up)
		}
		if !found {
			return errors.New("上游 " + name + " 不存在")
		}
		draft.Upstreams = filtered
		return nil
	}); err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	a.rt.Resolver().PurgeCache()
	logx.Infof("[adminapi] 已删除上游 %s", name)
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "upstreams": a.describeUpstreams(a.rt.Config().Upstreams)})
}

// handleTestUpstream 在保存之前试连一次，让用户当场知道地址或密钥是否可用。
func (a *API) handleTestUpstream(writer http.ResponseWriter, request *http.Request) {
	var payload upstreamPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}
	candidate := payload.toConfig(a.rt.Config().UpstreamByName(strings.TrimSpace(payload.Name)))
	// 试连不进入配置文件，但同样要过一遍校验，避免把明显错误的地址发出去。
	probe := &config.Config{Server: config.Server{Listen: ":0"}, Redirect: config.Redirect{Mode: config.RedirectAlways}, Upstreams: []config.Upstream{candidate}}
	if err := probe.Validate(); err != nil {
		writeJSON(writer, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	provider, err := upstream.New(probe.Upstreams[0])
	if err != nil {
		writeJSON(writer, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if !provider.HasCredentials() {
		writeJSON(writer, http.StatusOK, map[string]any{"ok": false, "error": "缺少 API 密钥，无法读取书库，也无法解析 strm"})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	label, err := provider.Ping(ctx)
	if err != nil {
		writeJSON(writer, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	libraries, libErr := provider.Libraries(ctx)
	response := map[string]any{"ok": true, "info": label, "libraries": libraries}
	if libErr != nil {
		response["warning"] = "连接成功，但读取书库失败: " + libErr.Error()
	}
	writeJSON(writer, http.StatusOK, response)
}

func (a *API) provider(request *http.Request) (upstream.Provider, error) {
	name := request.PathValue("name")
	provider := a.rt.ProviderByName(name)
	if provider == nil {
		return nil, errors.New("上游 " + name + " 未挂载（可能不存在或已停用）")
	}
	return provider, nil
}

func (a *API) handlePing(writer http.ResponseWriter, request *http.Request) {
	provider, err := a.provider(request)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	label, err := provider.Ping(ctx)
	if err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "info": label})
}

func (a *API) handleLibraries(writer http.ResponseWriter, request *http.Request) {
	provider, err := a.provider(request)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	libraries, err := provider.Libraries(ctx)
	if err != nil {
		writeError(writer, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"libraries": libraries})
}

func (a *API) handleItems(writer http.ResponseWriter, request *http.Request) {
	provider, err := a.provider(request)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	query := request.URL.Query()
	limit := intParam(query.Get("limit"), 50, 1, 500)
	page := intParam(query.Get("page"), 0, 0, 100000)

	ctx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
	defer cancel()
	items, total, err := provider.Items(ctx, query.Get("libraryId"), limit, page, strings.TrimSpace(query.Get("search")))
	if err != nil {
		writeError(writer, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items, "total": total, "limit": limit, "page": page})
}

func (a *API) handleItemFiles(writer http.ResponseWriter, request *http.Request) {
	provider, err := a.provider(request)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
	defer cancel()
	item, files, err := provider.ItemFiles(ctx, request.PathValue("itemId"))
	if err != nil {
		writeError(writer, http.StatusBadGateway, err.Error())
		return
	}

	// 给每个文件标注 AetherLink 实际会读取的容器路径，以及 .strm 指针的解析
	// 结果——这是排查「书库能看见但播不了」最有用的一屏。
	type annotatedFile struct {
		upstream.File
		ContainerPath string       `json:"containerPath,omitempty"`
		Target        *strm.Target `json:"target,omitempty"`
		Error         string       `json:"error,omitempty"`
		PlayPath      string       `json:"playPath"`
	}
	mapper := provider.Mapper()
	annotated := make([]annotatedFile, 0, len(files))
	for _, file := range files {
		entry := annotatedFile{File: file, PlayPath: provider.Prefix() + provider.PlaybackPath(item.ID, file.ID)}
		if file.IsStrm {
			containerPath, mapErr := mapper.Check(file.Path)
			if mapErr != nil {
				entry.Error = mapErr.Error()
			} else {
				entry.ContainerPath = containerPath
				target, readErr := strm.Read(containerPath, mapper)
				if readErr != nil {
					entry.Error = readErr.Error()
				} else {
					entry.Target = target
				}
			}
		}
		annotated = append(annotated, entry)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"item": item, "files": annotated})
}

// handleResolve 跑一遍完整解析流水线，便于在把播放器指向 AetherLink 之前先
// 验证 302 行为。
func (a *API) handleResolve(writer http.ResponseWriter, request *http.Request) {
	provider, err := a.provider(request)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	query := request.URL.Query()
	ref := upstream.MediaRef{
		Kind:          upstream.RefKind(defaultString(query.Get("kind"), string(upstream.RefLibraryFile))),
		ItemID:        query.Get("itemId"),
		FileID:        query.Get("fileId"),
		MediaSourceID: query.Get("mediaSourceId"),
		SessionID:     query.Get("sessionId"),
		TrackIndex:    query.Get("trackIndex"),
	}

	mediaResolver := a.rt.Resolver()
	ctx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
	defer cancel()
	resolution, cacheHit, err := mediaResolver.Resolve(ctx, provider, ref, request.UserAgent())
	if err != nil {
		if errors.Is(err, resolver.ErrNotStrm) {
			writeJSON(writer, http.StatusOK, map[string]any{"isStrm": false, "message": err.Error()})
			return
		}
		writeError(writer, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"isStrm":       true,
		"cacheHit":     cacheHit,
		"resolution":   resolution,
		"willRedirect": mediaResolver.ShouldRedirect(resolution),
		"playUrl":      resolution.PlayURL(),
	})
}

type parseStrmRequest struct {
	Content  string `json:"content"`
	BasePath string `json:"basePath"`
	Upstream string `json:"upstream"`
}

// handleParseStrm 不接触上游，直接解析原始 .strm 内容，用来验证中文路径、
// 115 pick code 这类特殊指针。
func (a *API) handleParseStrm(writer http.ResponseWriter, request *http.Request) {
	var payload parseStrmRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	if strings.TrimSpace(payload.Content) == "" {
		writeError(writer, http.StatusBadRequest, "content 不能为空")
		return
	}
	target, err := strm.Parse(payload.Content, payload.BasePath, a.mapperFor(payload.Upstream))
	if err != nil {
		writeJSON(writer, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "target": target})
}

// mapperFor 返回指定上游的路径映射器；传空表示不做根目录限制的裸解析。
func (a *API) mapperFor(upstreamName string) *pathmap.Mapper {
	if upstreamName == "" {
		return nil
	}
	if provider := a.rt.ProviderByName(upstreamName); provider != nil {
		return provider.Mapper()
	}
	return nil
}

func (a *API) handleStats(writer http.ResponseWriter, request *http.Request) {
	limit := intParam(request.URL.Query().Get("events"), 50, 1, 500)
	writeJSON(writer, http.StatusOK, a.rt.Stats().Snapshot(limit))
}

func (a *API) handleLogs(writer http.ResponseWriter, request *http.Request) {
	limit := intParam(request.URL.Query().Get("limit"), 200, 1, 1000)
	writeJSON(writer, http.StatusOK, map[string]any{"entries": logx.Recent(limit)})
}

func (a *API) handlePurgeCache(writer http.ResponseWriter, request *http.Request) {
	removed := a.rt.Resolver().PurgeCache()
	logx.Infof("[adminapi] 已清空解析缓存（%d 条）", removed)
	writeJSON(writer, http.StatusOK, map[string]any{"purged": removed})
}

// decodeJSON 解析请求体，出错时已写好响应并返回 false。
func decodeJSON(writer http.ResponseWriter, request *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeError(writer, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}

// parseDuration 接受 "5m" 这类写法，空字符串表示沿用旧值。
func parseDuration(raw string, fallback time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	if raw == "0" {
		return 0, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New("必须是时长，例如 5m、30s、0")
	}
	if value < 0 {
		return 0, errors.New("不能是负数")
	}
	return value, nil
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{"error": message})
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(payload); err != nil {
		logx.Debugf("[adminapi] 写响应失败: %v", err)
	}
}

func intParam(raw string, fallback, minimum, maximum int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
