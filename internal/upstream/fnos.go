package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// fnosProvider 反代飞牛 NAS 自带的「影视」应用。
//
// 飞牛影视本身就是一套 Emby 方言的服务端：播放路由（/Videos/:id/stream、
// /Items/:id/Download）、播放协商（/Items/:id/PlaybackInfo）与字段含义都和
// Emby 一致，所以拦截、解析、302 这些主体逻辑直接复用 embyProvider，
// 只在三处做飞牛特有的适配（这三处正是 LitePan 的 internal/fnosproxy 所做的事）：
//
//  1. API 调用要走 /emby 前缀。飞牛把接口挂在这个前缀下，少了它 POST/GET 会
//     落到 SPA 返回的 HTML 上，于是「看不到媒体源」→ 永远不 302。
//  2. PlaybackInfo 里的 MediaStreams 要补齐必填字段。飞牛返回的流信息会缺
//     少部分键或给 null，部分客户端遇到 null 会直接判定为不可播放。
//  3. 网页播放器 basehtmlplayer.js 里的 crossorigin="anonymous" 要去掉。
//     302 出去的是跨域直链，带着 anonymous 会让浏览器把播放请求当成需要
//     CORS 放行的请求，直链服务端不返回 CORS 头时视频就播不出来。
//
// 鉴权上飞牛和 Emby 也不同：它没有 Emby 控制台里那种静态 API 密钥，接口只认
// 客户端登录换来的令牌。所以账号密码是「填了更好、不填也能播」的可选项——
// 不填时 302 主链路照常（靠 PlaybackInfo 改写 + 缓存），填了才多出媒体库列表
// 和缓存未命中时的兜底解析。
type fnosProvider struct {
	embyProvider
}

var (
	// 飞牛网页播放器的脚本，前缀可能是 /emby 也可能没有。
	fnosBaseHTMLPlayerRe = regexp.MustCompile(`(?i)^(?:/[^/]+)*/web/modules/htmlvideoplayer/basehtmlplayer\.js$`)
	// 压缩后的原表达式与它展开后的写法都要覆盖，飞牛不同版本打包方式不同。
	fnosHTMLCrossOriginRe = regexp.MustCompile(`mediaSource\.IsRemote\s*&&\s*(?:"DirectPlay"\s*===\s*playMethod|playMethod\s*===\s*"DirectPlay")\s*\?\s*null\s*:\s*"anonymous"`)
	fnosCrossOriginSource = []byte(`mediaSource.IsRemote&&"DirectPlay"===playMethod?null:"anonymous"`)
	// 兜底守卫：有的版本这段逻辑埋得很深，正则抓不到时靠它把 crossOrigin
	// 属性整体废掉（读永远返回 null，写忽略）。整段用 try 包住，解析失败也
	// 不会影响播放器脚本自身。
	fnosCrossOriginGuard = []byte(`;try{(function(){var s=Element.prototype.setAttribute;Element.prototype.setAttribute=function(n,v){if(this&&this.tagName&&/^(VIDEO|AUDIO)$/i.test(this.tagName)&&String(n).toLowerCase()==='crossorigin')return;return s.call(this,n,v)};try{Object.defineProperty(HTMLMediaElement.prototype,'crossOrigin',{get:function(){return null},set:function(){return null},configurable:true})}catch(e){}})()}catch(e){};`)
)

// fnosMediaStreamNonNullFields 是部分客户端要求存在且非 null 的流信息字段。
var fnosMediaStreamNonNullFields = []string{"Type", "Language", "DisplayLanguage", "Title", "DisplayTitle"}

// normalizeFnosMediaStreams 把缺失或为 null 的流信息字段补成空串，
// 返回是否发生了改动。
func normalizeFnosMediaStreams(source map[string]any) bool {
	if source == nil {
		return false
	}
	streams, ok := source["MediaStreams"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, entry := range streams {
		stream, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range fnosMediaStreamNonNullFields {
			if value, exists := stream[field]; !exists || value == nil {
				stream[field] = ""
				changed = true
			}
		}
	}
	return changed
}

// RewriteRequestPath 把播放协商钉到 /emby 前缀下。
//
// 只调这一条路径，不碰其余请求：飞牛的网页界面与媒体字节接口都在根路径下
// （/web/...、/Videos/...），无差别加前缀会把界面打挂。PlaybackInfo 则相反，
// 它只有 /emby/Items/:id/PlaybackInfo 这一种形态，不带前缀时飞牛返回的是
// 单页应用的 HTML —— 拿不到 JSON 就没有 MediaSources，改写无从下手，
// 客户端看不到直放能力就会去走转码，AetherLink 也就再没机会 302。
func (p *fnosProvider) RewriteRequestPath(request *http.Request) string {
	requestPath := request.URL.Path
	if requestPath == "" || !embyPlaybackInfoRe.MatchString(path.Clean(requestPath)) {
		return requestPath
	}
	// 地址里已经写了 /emby 的用户交给代理去重，这里再补一次会拼成 /emby/emby。
	if fnosAPIPrefix(p.base.Path) == "" {
		return requestPath
	}
	if strings.HasPrefix(strings.ToLower(requestPath), "/emby/") {
		return requestPath
	}
	return "/emby" + requestPath
}

// WantsResponseRewrite 除了 Emby 的播放协商，还要拦飞牛的网页播放器脚本。
func (p *fnosProvider) WantsResponseRewrite(request *http.Request) bool {
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		return false
	}
	cleaned := path.Clean(request.URL.Path)
	return embyPlaybackInfoRe.MatchString(cleaned) || fnosBaseHTMLPlayerRe.MatchString(cleaned)
}

// RewriteResponse 按路径分流：播放器脚本走 crossOrigin 处理，
// 其余（PlaybackInfo）交给 Emby 的改写逻辑。
func (p *fnosProvider) RewriteResponse(originalPath string, response *http.Response) (int, error) {
	if fnosBaseHTMLPlayerRe.MatchString(path.Clean(originalPath)) {
		return rewriteFnosBaseHTMLPlayer(response)
	}
	return p.embyProvider.RewriteResponse(originalPath, response)
}

// Ping 先问不需要鉴权的 /System/Info/Public。
//
// Emby 默认探的 /System/Info 要带令牌，而飞牛在没配账号密码时不会给 ——
// 但飞牛无论如何都愿意回答 /System/Info/Public，返回的服务器名与版本和
// /System/Info 一致。所以先探它，没有这条路由的旧版本再退回 /System/Info。
func (p *fnosProvider) Ping(ctx context.Context) (string, error) {
	var info embySystemInfo
	err := p.client.getJSON(ctx, "/System/Info/Public", nil, &info)
	if err != nil {
		if err = p.client.getJSON(ctx, "/System/Info", nil, &info); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("飞牛影视 %s v%s", strings.TrimSpace(info.ServerName), strings.TrimSpace(info.Version)), nil
}

// Libraries 读飞牛的媒体库列表。
//
// 这条接口要求以某个用户身份登录，只填地址不填账号密码时上游只会回 401。
// 这时把「该填什么」补进错误里，比原样抛一串状态码有用得多；已经配了账号
// 密码却还失败，那就是真实故障，如实上报。
func (p *fnosProvider) Libraries(ctx context.Context) ([]Library, error) {
	libraries, err := p.embyProvider.Libraries(ctx)
	if err != nil && !p.client.authenticated() {
		return nil, fmt.Errorf("飞牛影视未配置登录账号，无法读取媒体库（%w）", err)
	}
	return libraries, err
}

// rewriteFnosBaseHTMLPlayer 去掉网页播放器给 media 元素加的 crossorigin 属性。
// 只动这一处，界面的其他脚本与样式照原样透传。
func rewriteFnosBaseHTMLPlayer(response *http.Response) (int, error) {
	if response == nil || response.Body == nil {
		return 0, nil
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return 0, nil
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	// 改写的前提是能拿到明文脚本；上游返回压缩内容时静态文件也不能保证解压
	// 后仍是同一个字节序列，直接放弃改写比改坏脚本安全。
	if encoding := strings.TrimSpace(response.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return 0, fmt.Errorf("上游返回了无法改写的 Content-Encoding %q", encoding)
	}

	rewritten := bytes.ReplaceAll(body, fnosCrossOriginSource, []byte("null"))
	rewritten = fnosHTMLCrossOriginRe.ReplaceAll(rewritten, []byte("null"))
	if !bytes.Contains(rewritten, []byte("HTMLMediaElement.prototype,'crossOrigin'")) {
		guarded := make([]byte, 0, len(fnosCrossOriginGuard)+len(rewritten))
		guarded = append(guarded, fnosCrossOriginGuard...)
		guarded = append(guarded, rewritten...)
		rewritten = guarded
	}
	if bytes.Equal(rewritten, body) {
		return 0, nil
	}

	response.Body = io.NopCloser(bytes.NewReader(rewritten))
	response.ContentLength = int64(len(rewritten))
	response.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	response.Header.Set("Cache-Control", "no-store")
	response.Header.Del("Content-Encoding")
	response.Header.Del("ETag")
	response.Header.Del("Content-MD5")
	return 1, nil
}

var (
	_ Provider            = (*fnosProvider)(nil)
	_ ResponseRewriter    = (*fnosProvider)(nil)
	_ RequestPathRewriter = (*fnosProvider)(nil)
)
