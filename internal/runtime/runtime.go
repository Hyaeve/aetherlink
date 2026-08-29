// Package runtime 把「配置」与「正在服务的组件」解耦，使管理界面改完配置后
// 无需重启容器即可生效。
//
// 由于 AetherLink 的全部配置都在 Web 页面里维护（compose 只挂一个 /config 卷），
// 运行期必须能安全地整体替换上游 provider、解析器和反向代理。这里用一个
// atomic.Pointer 持有不可变的 stack 快照：读路径（每个播放请求）完全无锁，
// 写路径（管理接口）串行化在一把互斥锁后面。
package runtime

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/logx"
	"github.com/aetherlink/aetherlink/internal/proxy"
	"github.com/aetherlink/aetherlink/internal/resolver"
	"github.com/aetherlink/aetherlink/internal/stats"
	"github.com/aetherlink/aetherlink/internal/upstream"
)

// stack 是一组彼此匹配的运行期组件，整体替换以避免出现半新半旧的状态。
type stack struct {
	cfg       *config.Config
	providers []upstream.Provider
	resolver  *resolver.Resolver
	proxy     *proxy.Server
}

// Runtime 协调配置与运行期组件。
type Runtime struct {
	// applyMu 串行化所有配置变更。
	applyMu sync.Mutex
	current atomic.Pointer[stack]

	// stats 跨重载保留，这样改一次配置不会把统计清零。
	stats *stats.Collector

	configPath string
	// bootListen 是进程启动时实际监听的地址，用于判断是否需要重启容器。
	bootListen string
}

// New 按初始配置构建运行时。
func New(cfg *config.Config, collector *stats.Collector) (*Runtime, error) {
	if cfg == nil {
		return nil, errors.New("配置不能为空")
	}
	rt := &Runtime{
		stats:      collector,
		configPath: cfg.Path(),
		bootListen: cfg.Server.Listen,
	}
	built, err := rt.build(cfg)
	if err != nil {
		return nil, err
	}
	rt.current.Store(built)
	return rt, nil
}

// build 由一份配置构建全套组件，任何上游初始化失败都不会影响现有 stack。
func (rt *Runtime) build(cfg *config.Config) (*stack, error) {
	providers := make([]upstream.Provider, 0, len(cfg.Upstreams))
	for _, upstreamCfg := range cfg.Upstreams {
		if !upstreamCfg.IsEnabled() {
			logx.Infof("[runtime] 上游 %s 已停用，跳过", upstreamCfg.Name)
			continue
		}
		provider, err := upstream.New(upstreamCfg)
		if err != nil {
			return nil, fmt.Errorf("初始化上游 %s 失败: %w", upstreamCfg.Name, err)
		}
		if !provider.HasCredentials() {
			logx.Warnf("[runtime] 上游 %s 未配置 API 密钥：请求会被原样反代，不做 strm 解析", provider.Name())
		}
		providers = append(providers, provider)
	}

	mediaResolver := resolver.New(cfg.Cache, cfg.Redirect)
	return &stack{
		cfg:       cfg,
		providers: providers,
		resolver:  mediaResolver,
		proxy:     proxy.New(providers, mediaResolver, rt.stats, cfg.Redirect),
	}, nil
}

// Config 返回当前生效配置的深拷贝，调用方可以随意修改。
func (rt *Runtime) Config() *config.Config { return rt.current.Load().cfg.Clone() }

// Resolver 返回当前解析器。
func (rt *Runtime) Resolver() *resolver.Resolver { return rt.current.Load().resolver }

// Proxy 返回当前反向代理。
func (rt *Runtime) Proxy() *proxy.Server { return rt.current.Load().proxy }

// Stats 返回跨重载持久的统计收集器。
func (rt *Runtime) Stats() *stats.Collector { return rt.stats }

// ConfigPath 返回配置文件路径。
func (rt *Runtime) ConfigPath() string { return rt.configPath }

// RestartRequired 报告监听地址是否被改成了与启动时不同的值。监听套接字无法热
// 替换，所以只能提示用户重启容器。
func (rt *Runtime) RestartRequired() bool {
	return rt.current.Load().cfg.Server.Listen != rt.bootListen
}

// BootListen 返回进程实际监听的地址。
func (rt *Runtime) BootListen() string { return rt.bootListen }

// RootUpstreamMounted 报告是否有上游挂在根路径上。挂在根上时裸访问 / 属于该
// 上游（例如 Audiobookshelf 自己的网页），不能抢来跳转到管理界面。
func (rt *Runtime) RootUpstreamMounted() bool {
	for _, upstreamCfg := range rt.current.Load().cfg.Upstreams {
		if upstreamCfg.IsEnabled() && upstreamCfg.Prefix == "/" {
			return true
		}
	}
	return false
}

// ProviderByName 在当前 stack 中查找上游。
func (rt *Runtime) ProviderByName(name string) upstream.Provider {
	return rt.current.Load().proxy.ProviderByName(name)
}

// ServeHTTP 把请求交给当前反向代理，使热重载对调用方透明。
func (rt *Runtime) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	rt.current.Load().proxy.ServeHTTP(writer, request)
}

// Apply 以「克隆 → 修改 → 校验 → 构建 → 落盘 → 切换」的顺序更新配置。
// 只有全部成功才会切换，因此校验失败或上游地址非法时服务继续按旧配置运行。
func (rt *Runtime) Apply(mutate func(draft *config.Config) error) error {
	rt.applyMu.Lock()
	defer rt.applyMu.Unlock()

	draft := rt.current.Load().cfg.Clone()
	if err := mutate(draft); err != nil {
		return err
	}
	if err := draft.Validate(); err != nil {
		return err
	}
	built, err := rt.build(draft)
	if err != nil {
		return err
	}
	// 先落盘再切换：如果 /config 不可写，用户应立刻看到报错，而不是得到一个
	// 重启后就消失的「已保存」假象。
	if err := draft.Save(rt.configPath); err != nil {
		return fmt.Errorf("写入配置文件 %s 失败: %w", rt.configPath, err)
	}
	rt.current.Store(built)

	logx.SetLevel(logx.ParseLevel(draft.Server.LogLevel))
	logx.SetMaxEntries(draft.Server.LogBuffer)
	logx.Infof("[runtime] 配置已重载：%d 个上游生效，跳转模式 %s", len(built.providers), draft.Redirect.Mode)
	return nil
}
