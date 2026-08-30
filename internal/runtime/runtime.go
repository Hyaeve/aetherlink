// Package runtime 把「配置」与「正在服务的组件」解耦，使管理界面改完配置后
// 无需重启容器即可生效。
//
// 由于 AetherLink 的全部配置都在 Web 页面里维护（compose 只挂一个 /config 卷），
// 运行期必须能安全地整体替换上游 provider、解析器和反向代理。这里用一个
// atomic.Pointer 持有不可变的 stack 快照：读路径（每个播放请求）完全无锁，
// 写路径（管理接口）串行化在一把互斥锁后面。
//
// 每个上游各自监听一个端口（原地址端口 → 反代端口），因此 runtime 还负责按
// 配置增删这些监听器：新增上游立刻开始监听，删除或停用就关掉对应端口。
package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/logx"
	"github.com/aetherlink/aetherlink/internal/proxy"
	"github.com/aetherlink/aetherlink/internal/resolver"
	"github.com/aetherlink/aetherlink/internal/stats"
	"github.com/aetherlink/aetherlink/internal/upstream"
)

// stack 是一组彼此匹配的运行期组件，整体替换以避免出现半新半旧的状态。
type stack struct {
	cfg      *config.Config
	resolver *resolver.Resolver
	// proxies 按容器端口索引：端口就是上游的唯一入口。
	proxies map[int]*proxy.Server
}

func (s *stack) providerByName(name string) upstream.Provider {
	for _, server := range s.proxies {
		if server.Provider().Name() == name {
			return server.Provider()
		}
	}
	return nil
}

// portListener 是某个端口上正在运行的 HTTP 服务。
type portListener struct {
	port     int
	listener net.Listener
	server   *http.Server
}

// Runtime 协调配置与运行期组件。
type Runtime struct {
	// applyMu 串行化所有配置变更与端口增删。
	applyMu sync.Mutex
	current atomic.Pointer[stack]

	// stats 跨重载保留，这样改一次配置不会把统计清零。
	stats *stats.Collector

	configPath string
	// bootListen 是进程启动时实际监听的管理地址，用于判断是否需要重启容器。
	bootListen string

	listenersMu sync.Mutex
	listeners   map[int]*portListener
	// serving 为真之后才会真正绑定端口。单元测试不调用 Start，
	// 因此不会占用真实端口。
	serving bool
}

// New 按初始配置构建运行时。端口监听要等到 Start 才建立。
func New(cfg *config.Config, collector *stats.Collector) (*Runtime, error) {
	if cfg == nil {
		return nil, errors.New("配置不能为空")
	}
	rt := &Runtime{
		stats:      collector,
		configPath: cfg.Path(),
		bootListen: cfg.Server.Listen,
		listeners:  map[int]*portListener{},
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
	mediaResolver := resolver.New(cfg.Cache, cfg.Redirect)
	proxies := make(map[int]*proxy.Server, len(cfg.Upstreams))
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
		redirectCfg := cfg.Redirect
		if upstreamCfg.RedirectMode != "" {
			redirectCfg.Mode = upstreamCfg.RedirectMode
		}
		if redirectCfg.Mode == config.RedirectAlways {
			redirectCfg.AllowPublicTargets = config.Bool(true)
		}
		proxies[upstreamCfg.ListenPort] = proxy.New(provider, mediaResolver, rt.stats, redirectCfg)
	}
	return &stack{cfg: cfg, resolver: mediaResolver, proxies: proxies}, nil
}

// Config 返回当前生效配置的深拷贝，调用方可以随意修改。
func (rt *Runtime) Config() *config.Config { return rt.current.Load().cfg.Clone() }

// Resolver 返回当前解析器。
func (rt *Runtime) Resolver() *resolver.Resolver { return rt.current.Load().resolver }

// Stats 返回跨重载持久的统计收集器。
func (rt *Runtime) Stats() *stats.Collector { return rt.stats }

// ConfigPath 返回配置文件路径。
func (rt *Runtime) ConfigPath() string { return rt.configPath }

// RestartRequired 报告管理端口是否被改成了与启动时不同的值。管理端口的套接字
// 无法热替换，只能提示重启容器；上游端口不受此限制，可以随时增删。
func (rt *Runtime) RestartRequired() bool {
	return rt.current.Load().cfg.Server.Listen != rt.bootListen
}

// BootListen 返回进程实际监听的管理地址。
func (rt *Runtime) BootListen() string { return rt.bootListen }

// ProviderByName 在当前 stack 中查找上游。
func (rt *Runtime) ProviderByName(name string) upstream.Provider {
	return rt.current.Load().providerByName(name)
}

// PortActive 报告某个上游端口是否已经在监听。界面用它区分「已配置」与
// 「真的在服务」。
func (rt *Runtime) PortActive(port int) bool {
	rt.listenersMu.Lock()
	defer rt.listenersMu.Unlock()
	_, ok := rt.listeners[port]
	return ok
}

// handlerFor 返回某端口的请求入口。它每次都从当前 stack 取代理，因此改配置后
// 无需重建监听器就能生效。
func (rt *Runtime) handlerFor(port int) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		server := rt.current.Load().proxies[port]
		if server == nil {
			http.Error(writer, "AetherLink: 该端口上没有启用的上游", http.StatusServiceUnavailable)
			return
		}
		server.ServeHTTP(writer, request)
	})
}

// Start 为当前配置里的每个启用上游绑定端口并开始服务。
func (rt *Runtime) Start() error {
	rt.applyMu.Lock()
	defer rt.applyMu.Unlock()

	rt.listenersMu.Lock()
	rt.serving = true
	rt.listenersMu.Unlock()

	commit, _, err := rt.acquirePorts(rt.current.Load())
	if err != nil {
		return err
	}
	commit()
	return nil
}

// Shutdown 关闭全部上游监听端口。
func (rt *Runtime) Shutdown(ctx context.Context) {
	rt.listenersMu.Lock()
	listeners := rt.listeners
	rt.listeners = map[int]*portListener{}
	rt.serving = false
	rt.listenersMu.Unlock()

	for _, entry := range listeners {
		if err := entry.server.Shutdown(ctx); err != nil {
			logx.Debugf("[runtime] 关闭端口 %d 时出错: %v", entry.port, err)
		}
	}
}

// acquirePorts 先把目标 stack 需要的新端口全部绑定下来，成功后返回 commit 与
// rollback 两个函数：commit 真正开始服务并关掉多余端口，rollback 释放刚拿到的
// 监听器。
//
// 拆成两段是为了让「端口已被占用」这类错误在配置落盘之前就暴露出来，并且在落盘
// 失败时把端口原样交回去；无论哪种失败，现有端口都一个不动。
// 调用方必须持有 applyMu。
func (rt *Runtime) acquirePorts(target *stack) (commit func(), rollback func(), err error) {
	rt.listenersMu.Lock()
	serving := rt.serving
	existing := make(map[int]*portListener, len(rt.listeners))
	for port, entry := range rt.listeners {
		existing[port] = entry
	}
	rt.listenersMu.Unlock()

	if !serving {
		return func() {}, func() {}, nil
	}

	added := map[int]*portListener{}
	for port := range target.proxies {
		if _, ok := existing[port]; ok {
			continue
		}
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			for _, pending := range added {
				pending.listener.Close()
			}
			return nil, nil, fmt.Errorf("端口 %d 无法监听（可能已被其他程序占用）: %w", port, err)
		}
		added[port] = &portListener{port: port, listener: listener}
	}

	stale := make([]*portListener, 0)
	for port, entry := range existing {
		if _, ok := target.proxies[port]; !ok {
			stale = append(stale, entry)
		}
	}

	commit = func() {
		rt.listenersMu.Lock()
		for port, entry := range added {
			entry.server = &http.Server{
				Handler: rt.handlerFor(port),
				// 有意不设读写超时：媒体流是长连接，中途被切断会导致播放中断。
				ReadHeaderTimeout: 20 * time.Second,
				IdleTimeout:       120 * time.Second,
			}
			rt.listeners[port] = entry
		}
		for _, entry := range stale {
			delete(rt.listeners, entry.port)
		}
		rt.listenersMu.Unlock()

		for _, entry := range added {
			go func(entry *portListener) {
				logx.Infof("[runtime] 端口 %d 开始反代", entry.port)
				if err := entry.server.Serve(entry.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logx.Errorf("[runtime] 端口 %d 服务中断: %v", entry.port, err)
				}
			}(entry)
		}
		for _, entry := range stale {
			logx.Infof("[runtime] 端口 %d 已停止反代", entry.port)
			if err := entry.server.Close(); err != nil {
				logx.Debugf("[runtime] 关闭端口 %d 时出错: %v", entry.port, err)
			}
		}
	}
	rollback = func() {
		for _, pending := range added {
			pending.listener.Close()
		}
	}
	return commit, rollback, nil
}

// Apply 以「克隆 → 修改 → 校验 → 构建 → 占端口 → 落盘 → 切换 → 启停端口」的
// 顺序更新配置。任何一步失败都直接返回，运行中的服务与磁盘上的文件保持原样。
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
	// 端口先占下来：写错的端口应当立刻报错，而不是保存成功后才发现没在服务。
	commit, rollback, err := rt.acquirePorts(built)
	if err != nil {
		return err
	}
	// 再落盘：如果 /config 不可写，用户应立刻看到报错，而不是得到一个重启后
	// 就消失的「已保存」假象。落盘失败时把刚占下的端口交回去。
	if err := draft.Save(rt.configPath); err != nil {
		rollback()
		return fmt.Errorf("写入配置文件 %s 失败: %w", rt.configPath, err)
	}
	rt.current.Store(built)
	commit()

	logx.SetLevel(logx.ParseLevel(draft.Server.LogLevel))
	logx.SetMaxEntries(draft.Server.LogBuffer)
	logx.Infof("[runtime] 配置已重载：%d 个上游生效，跳转模式 %s", len(built.proxies), draft.Redirect.Mode)
	return nil
}
