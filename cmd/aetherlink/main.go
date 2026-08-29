// Command aetherlink 启动 AetherLink 反向代理：它挡在 Audiobookshelf / Emby
// 前面，识别播放请求背后的 .strm 指针，并用 302 把播放器直接指向真实媒体地址。
//
// 除监听地址以外的所有配置都在网页上维护并写回 /config/config.yaml，
// 因此 docker compose 只需要挂载一个 /config 卷。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aetherlink/aetherlink/internal/adminapi"
	"github.com/aetherlink/aetherlink/internal/auth"
	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/logx"
	"github.com/aetherlink/aetherlink/internal/runtime"
	"github.com/aetherlink/aetherlink/internal/stats"
	"github.com/aetherlink/aetherlink/internal/web"
)

// version 在构建时用 -ldflags "-X main.version=..." 覆盖。
var version = "dev"

func main() {
	configPath := flag.String("config", defaultConfigPath(), "AetherLink 配置文件路径")
	printVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	if *printVersion {
		fmt.Println("aetherlink", version)
		return
	}

	adminapi.Version = version

	// 配置文件不存在时自动创建：新装实例不需要人工准备任何文件，
	// 直接打开网页设置管理口令即可。
	cfg, created, err := config.LoadOrCreate(*configPath)
	if err != nil {
		logx.Errorf("加载配置失败: %v", err)
		// 权限问题是绑定挂载最常见的坑：宿主的 ./config 属于 root，
		// 容器里的非 root 进程写不进去。直接把处置办法打出来，
		// 避免用户只看到一个反复重启的容器。
		if errors.Is(err, os.ErrPermission) {
			logx.Errorf("当前用户 uid=%d gid=%d 对配置目录没有写权限", os.Getuid(), os.Getgid())
			logx.Errorf("请在宿主上执行：chown -R %d:%d ./config，或去掉 compose 里的 user: 让入口脚本自动修正属主", os.Getuid(), os.Getgid())
		}
		os.Exit(1)
	}
	if created {
		logx.Infof("已在 %s 创建默认配置", *configPath)
	}

	// 没有账号时补上内置的 admin/password，省掉初始化向导：打开页面直接登录。
	// 这个分支也覆盖了从旧版本升级、以及用户手工清空 auth 段的情况。
	if !cfg.Auth.IsConfigured() {
		defaults, deriveErr := auth.Default()
		if deriveErr != nil {
			logx.Errorf("生成默认账号失败: %v", deriveErr)
			os.Exit(1)
		}
		cfg.Auth = defaults
		if saveErr := cfg.Save(*configPath); saveErr != nil {
			logx.Errorf("写入默认账号失败: %v", saveErr)
			os.Exit(1)
		}
		logx.Warnf("已启用默认账号 %s / %s，请登录后在设置页里修改", auth.DefaultUsername, auth.DefaultPassword)
	}

	logx.SetLevel(logx.ParseLevel(cfg.Server.LogLevel))
	logx.SetMaxEntries(cfg.Server.LogBuffer)

	collector := stats.New(500)
	rt, err := runtime.New(cfg, collector)
	if err != nil {
		logx.Errorf("初始化运行时失败: %v", err)
		os.Exit(1)
	}
	for _, upstreamCfg := range cfg.Upstreams {
		if upstreamCfg.IsEnabled() {
			logx.Infof("上游 %s (%s) 挂载于 %s -> %s", upstreamCfg.Name, upstreamCfg.Type, upstreamCfg.Prefix, upstreamCfg.BaseURL)
		}
	}

	sessions := auth.NewStore(auth.DefaultSessionTTL)
	admin := adminapi.New(rt, sessions)

	if cfg.Auth.DefaultCredentials {
		logx.Warnf("管理界面 http://<主机地址>:%s/ 仍在使用默认账号，请尽快修改", portOf(cfg.Server.Listen))
	}
	if strings.TrimSpace(cfg.Server.AdminToken) != "" {
		logx.Warnf("检测到应急令牌 AETHERLINK_ADMIN_TOKEN：它可以绕过口令登录，排障完成后请移除")
	}

	mux := http.NewServeMux()
	mux.Handle(adminapi.BasePath+"/", admin.Handler())
	mux.Handle(web.MountPath, web.Handler())
	// 裸的 /aetherlink 重定向到界面根路径，省得用户手敲末尾斜杠。
	mux.HandleFunc("/aetherlink", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, web.MountPath, http.StatusMovedPermanently)
	})
	// 其余路径都属于被反代的上游。交给 runtime 而不是某个具体 proxy 实例，
	// 这样在网页上增删上游后无需重启即可生效。
	// 没有上游挂在根路径时，裸访问 / 直接送进管理界面，省得手敲 /aetherlink/。
	mux.Handle("/", rootHandler(rt))

	server := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: mux,
		// 有意不设读写超时：媒体流是长连接，中途被切断会导致播放中断。
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logx.Infof("AetherLink %s 正在监听 %s（管理界面 %s）", version, cfg.Server.Listen, web.MountPath)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logx.Errorf("HTTP 服务出错: %v", err)
			stop()
		}
	}()

	<-shutdownCtx.Done()
	logx.Infof("正在关闭")
	gracefulCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(gracefulCtx); err != nil {
		logx.Warnf("优雅关闭失败: %v", err)
	}
}

// rootHandler 让 http://主机:端口/ 直接落到管理界面，不必记住 /aetherlink/ 后缀。
// 有上游挂在根路径（prefix 为 /）时，根路径属于那个上游，不能抢：此时仍然只有
// /aetherlink/ 是管理界面。
func rootHandler(rt *runtime.Runtime) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/" && !rt.RootUpstreamMounted() {
			http.Redirect(writer, request, web.MountPath, http.StatusFound)
			return
		}
		rt.ServeHTTP(writer, request)
	})
}

// defaultConfigPath 优先使用容器内的挂载点，本地开发时退回当前目录。
func defaultConfigPath() string {
	if value := strings.TrimSpace(os.Getenv("AETHERLINK_CONFIG")); value != "" {
		return value
	}
	if _, err := os.Stat("/config"); err == nil {
		return "/config/config.yaml"
	}
	return "config.yaml"
}

// portOf 从监听地址里取出端口，仅用于日志里给出可点击的地址提示。
func portOf(listen string) string {
	if index := strings.LastIndex(listen, ":"); index >= 0 {
		return listen[index+1:]
	}
	return listen
}
