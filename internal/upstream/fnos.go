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
// 只在四处做飞牛特有的适配（前三处正是 LitePan 的 internal/fnosproxy 所做的事）：
//
//  1. API 调用要走 /emby 前缀。飞牛把接口挂在这个前缀下，少了它 POST/GET 会
//     落到 SPA 返回的 HTML 上，于是「看不到媒体源」→ 永远不 302。
//  2. 取条目要走播放协商 /Items/{id}/PlaybackInfo。飞牛的 Emby 兼容层只实现了
//     客户端真正会调的那几条路由：/Items 与 /Items/{id} 都回单页应用的 HTML
//     （HTTP 200 + 一整页 <!doctype html>），json 解析必然失败，整条解析链路
//     就此断掉，表现成「客户端能进库、能浏览，但一播放就失败」。PlaybackInfo
//     返回的 MediaSources 与条目详情里的同一份，足以承接解析（见
//     embyProvider.itemLookupOrder）。
//  3. PlaybackInfo 里的 MediaStreams 要补齐必填字段。飞牛返回的流信息会缺
//     少部分键或给 null，部分客户端遇到 null 会直接判定为不可播放。
//  4. 网页播放器 basehtmlplayer.js 里的 crossorigin="anonymous" 要去掉。
//     302 出去的是跨域直链，带着 anonymous 会让浏览器把播放请求当成需要
//     CORS 放行的请求，直链服务端不返回 CORS 头时视频就播不出来。
//
// 鉴权上飞牛和 Emby 也不同：它没有 Emby 控制台里那种静态 API 密钥，接口只认
// 客户端登录换来的令牌。而且令牌必须挂在完整的 X-Emby-Authorization 身份头上，
// Emby 那种 X-Emby-Token 简写形态会被 400 挡掉（见 apiClient.embyClientAuth）。
// 所以账号密码是「填了更好、不填也能播」的可选项——不填时 302 主链路照常
// （靠 PlaybackInfo 改写 + 缓存），填了才多出媒体库列表和缓存未命中时的兜底解析。
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
// 飞牛无论如何都愿意回答 /System/Info/Public，返回的服务器名与版本和
// /System/Info 一致。所以先探它，没有这条路由的旧版本再退回 /System/Info。
//
// 两条都失败时，若这个上游根本没配账号密码，就把「该填什么」补进错误里：
// 飞牛的拒绝语是「X-Emby-Authorization is missing」，原样抛给用户完全看不出
// 该做什么。配了账号还失败就是真实故障，如实上报。
func (p *fnosProvider) Ping(ctx context.Context) (string, error) {
	var info embySystemInfo
	err := p.client.getJSON(ctx, "/System/Info/Public", nil, &info)
	if err != nil {
		if err = p.client.getJSON(ctx, "/System/Info", nil, &info); err != nil {
			if !p.client.authenticated() {
				return "", fmt.Errorf("飞牛影视需要先填写登录账号与密码：它没有静态密钥，未登录时连服务信息都读不到（%w）", err)
			}
			return "", err
		}
	}
	return fmt.Sprintf("飞牛影视 %s v%s", strings.TrimSpace(info.ServerName), strings.TrimSpace(info.Version)), nil
}

// Libraries 读飞牛的媒体库列表。
//
// 先走 Emby 的 /Library/VirtualFolders。它在 Emby 里是管理员接口，用飞牛的普通
// 账号登录时会被拒；这种账号能读的是用户级的 /Library/SelectableMediaFolders
// （扁平数组，参考项目 LitePan 列库用的也是它）。所以再兜一次，两条都失败才
// 认账并报第一条错误 —— 凭据不对时，那一条才是准确的解释。
//
// 没有账号密码时上游只会回 401，这时把「该填什么」补进错误里，比原样抛一串
// 状态码有用得多；已经配了账号密码却还失败，那就是真实故障，如实上报。
func (p *fnosProvider) Libraries(ctx context.Context) ([]Library, error) {
	libraries, err := p.embyProvider.Libraries(ctx)
	if err == nil {
		return libraries, nil
	}
	if fallback, fallbackErr := p.selectableLibraries(ctx); fallbackErr == nil {
		return fallback, nil
	}
	if !p.client.authenticated() {
		return nil, fmt.Errorf("飞牛影视需要登录账号与密码才能读取媒体库（%w）", err)
	}
	return nil, err
}

// selectableLibraries 走用户级接口 /Library/SelectableMediaFolders，
// 响应是一个 {Id,Name,CollectionType} 的扁平数组。
func (p *fnosProvider) selectableLibraries(ctx context.Context) ([]Library, error) {
	var folders []struct {
		ID             string `json:"Id"`
		Name           string `json:"Name"`
		CollectionType string `json:"CollectionType"`
	}
	if err := p.client.getJSON(ctx, "/Library/SelectableMediaFolders", nil, &folders); err != nil {
		return nil, err
	}
	libraries := make([]Library, 0, len(folders))
	for _, folder := range folders {
		id := strings.TrimSpace(folder.ID)
		name := strings.TrimSpace(folder.Name)
		// 这条接口偶尔会夹带没有 Id 的占位项，丢掉它们。
		if id == "" || name == "" {
			continue
		}
		libraries = append(libraries, Library{
			ID:        id,
			Name:      name,
			MediaType: folder.CollectionType,
			Provider:  string(p.kind),
		})
	}
	return libraries, nil
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
