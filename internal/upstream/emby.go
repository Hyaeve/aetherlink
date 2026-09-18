package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/logx"
)

// embyProvider speaks the Emby (and Jellyfin-compatible) HTTP API using an
// API key issued in the Emby dashboard. The key is sent as X-Emby-Token and as
// the api_key query parameter, which is what Emby accepts for admin-level calls.
type embyProvider struct {
	providerBase

	// userMu 保护下面两个字段：PlaybackInfo 需要一个 UserId，查到之后缓存复用。
	userMu       sync.Mutex
	userLookedUp bool
	userID       string

	// PlaybackInfo 已经给过客户端的 STRM 媒体源短暂缓存下来。客户端下一步
	// 请求 /stream 时优先复用，避免某些 Emby 版本的 /Items 又把 URL 隐去。
	playbackMu      sync.Mutex
	playbackSources map[string]cachedEmbyMediaSource

	// normalizeSource 是方言扩展点：PlaybackInfo 里每个媒体源都会先过一遍它，
	// 用来补齐该方言特有的字段（飞牛影视要给 MediaStreams 补非空值）。
	// 返回是否改动了这个源对象。nil 表示该方言没有额外要求。
	normalizeSource func(source map[string]any) bool

	// preferItemByID 让方言优先用单项路由 /Items/{id} 取条目，而不是集合路由
	// /Items?Ids=。飞牛影视只实现了前者，后者在它上面回的是单页应用的 HTML。
	preferItemByID bool

	// claimsSources 表示卡片模式要求「流量全经 AetherLink」（「始终跳转」与
	// 「始终中继」）：上游判可直放的 STRM 源也要压成 DirectStream 引回我们的
	// /stream 路由，否则客户端会拿 DirectPlay 直连 Path 绕开 AetherLink。
	// 触发条件见 claimsSources。
	claimsSources bool

	// ignoresVerdict 表示上游「当前客户端不能直接播放原始文件」的判定也要无视，
	// 强制把源引回 AetherLink。它由两件事共同决定：模式要求流量经我们
	// （claimsSources），且卡片上的「无视上游的不可直放判定」开关是开着的
	// （config.Upstream.ShouldIgnoreDirectPlayVerdict，默认开）。
	//
	// 为什么要留这个开关：有些客户端只认上游的转码 HLS（实测 AfuseKt 就是这样，
	// 它拿到原始文件的直链后读几十 KB 就断开），对它们而言上游那句判定是唯一能
	// 放动的退路；关掉开关即可让这类客户端回到上游转码，而不是被强行塞原文件。
	ignoresVerdict bool

	// forceDelivery 是强制接入后字节的去向（「302」或「中继」），只进日志。
	// 中文日志里那句「强制让 N 个 STRM 媒体源接入 302」是中继模式下排障最先看
	// 到的一行，写错方向比不写更坏。
	forceDelivery string
}

// claimsSources 报告该跳转模式是否要求「流量全经 AetherLink」。
//
// 「始终跳转」与「始终中继」是用户显式指定流量去向的两端：前者要求 302 出去，
// 后者要求 AetherLink 亲自把字节投给客户端。两者都要求客户端先回到 AetherLink
// 的 /stream 路由——上游一句 SupportsDirectPlay=false 就足以把客户端甩去上游
// 转码 HLS，卡片上选的模式等于作废。
//
// 「公网跳转」「内网跳转」不同：它们只是在 302 与中继之间按客户端来源二选一，
// 上游的不可直放判定（码率限制、编码不支持等）该被采纳——网页端这类真的解不了
// 原始文件的客户端，无视判定会让它从「上游转码能播」变成「拿到放不动的原文件」。
func claimsSources(mode config.RedirectMode) bool {
	return mode == config.RedirectAlways || mode == config.RedirectNever
}

// forcedDelivery 给出强制接入后字节的去向，只用于日志。
func forcedDelivery(mode config.RedirectMode) string {
	if mode == config.RedirectNever {
		return "中继"
	}
	return "302"
}

type cachedEmbyMediaSource struct {
	source            embyMediaSource
	directPlayAllowed bool
	expiresAt         time.Time
}

const (
	embyPlaybackSourceTTL = 10 * time.Minute
	embyPlaybackSourceMax = 2048
)

// 前缀写成 (?:/[^/]+)? 而不是只认 /emby/：Emby 既可能装在反向代理的子路径下，
// 也有客户端会带上 /emby 前缀，只认死一种就会整条漏匹配。
var (
	// [/任意层级前缀]/Videos/:id/stream(.ext)、/Audio/:id/universal 等直放入口
	embyStreamRe = regexp.MustCompile(`(?i)^(?:/[^/]+)*/(?:videos|audio)/([^/]+)/(?:stream|original|universal)(?:\.[A-Za-z0-9]+)?$`)
	// [/任意层级前缀]/Items/:id/Download、/Items/:id/File
	embyDownloadRe = regexp.MustCompile(`(?i)^(?:/[^/]+)*/items/([^/]+)/(?:download|file)$`)
	// 任意层级前缀下的 /Items/:id/PlaybackInfo。客户端通常 POST 这条接口，
	// 根据返回的 Supports* 字段决定直放还是请求 hls1/main/*.ts。
	embyPlaybackInfoRe = regexp.MustCompile(`(?i)^(.*?)/items/([^/]+)/playbackinfo$`)
	embyContainerRe    = regexp.MustCompile(`^[a-z0-9]+$`)
)

// WantsResponseRewrite 判断这是不是 Emby 的播放协商响应。只改这一条 JSON
// 接口，界面和其他 API 仍然逐字节透传。
func (p *embyProvider) WantsResponseRewrite(request *http.Request) bool {
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		return false
	}
	return embyPlaybackInfoRe.MatchString(path.Clean(request.URL.Path))
}

// RewriteResponse 只把 Emby 已判定为可直接播放的 STRM 接到 AetherLink 直放
// 路由。若客户端不支持原始编码、容器或码率，必须保留 Emby 的转码能力；强行
// 把原文件交出去只会让客户端放不动，结果就是「有跳转却无法播放」。
//
// 例外是卡片的「始终跳转」与「始终中继」（claimsSources）：用户明确要求流量全经
// AetherLink 再 302 或中继，此时不再按上游判定分流，STRM 源一律压成 DirectStream
// 引回 AetherLink 的 /stream 路由。其中「上游说不能直放」的那一半还要卡片上的
// 「无视上游的不可直放判定」开关也开着（默认开，见 ignoresVerdict）——关掉它，
// 只认上游转码 HLS 的客户端才有活路。
func (p *embyProvider) RewriteResponse(originalPath string, response *http.Response) (int, error) {
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || response.Body == nil {
		return 0, nil
	}

	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if encoding := strings.TrimSpace(response.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return 0, fmt.Errorf("上游返回了无法改写的 Content-Encoding %q", encoding)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0, fmt.Errorf("解析 PlaybackInfo JSON: %w", err)
	}
	rawSources, ok := envelope["MediaSources"]
	if !ok {
		return 0, nil
	}
	var sources []map[string]any
	if err := json.Unmarshal(rawSources, &sources); err != nil {
		return 0, fmt.Errorf("解析 PlaybackInfo.MediaSources: %w", err)
	}

	matches := embyPlaybackInfoRe.FindStringSubmatch(path.Clean(originalPath))
	if matches == nil {
		return 0, nil
	}
	itemID, err := url.PathUnescape(matches[2])
	if err != nil {
		itemID = matches[2]
	}
	prefix := strings.TrimRight(matches[1], "/")
	changed := 0
	// mutated 记录响应体是否需要回写：方言补齐字段也算改动，但它不该被算进
	// 「接入 AetherLink 的媒体源数量」，否则日志会把没跳转的情况说成跳转了。
	mutated := false
	forced := 0
	// reclaimed 记「上游判可直放、却被压成 DirectStream 引回 AetherLink」的数量：
	// 这些源以前会让客户端直连原始地址，流量根本不经我们。
	reclaimed := 0
	skipped := make([]string, 0)
	for _, source := range sources {
		if p.normalizeSource != nil && p.normalizeSource(source) {
			mutated = true
		}
		if !isEmbyStrmPlaybackSource(source) || embyBool(source, "IsInfiniteStream") {
			continue
		}
		directPlayAllowed := embyAllowsDirectPlay(source)
		// claim 为真时把源引回 AetherLink。上游判可直放时必须引（DirectPlay 会让
		// 客户端直连媒体的 Path——飞牛场景就是网盘 / OpenList 直链——于是绕开
		// AetherLink，既没有播放记录，也不受内网地址安全网保护）；上游判不可直放
		// 时只有卡片开关也开着才引，否则把这句判定让给上游，客户端继续走上游转码。
		//
		// 强制的是 DirectStream 而不是 DirectPlay：DirectStream 把客户端引回
		// /stream，DirectPlay 则会直连原始地址。
		if p.claimsSources && (directPlayAllowed || p.ignoresVerdict) {
			if !directPlayAllowed {
				// 上游的不可直放判定（码率限制、远程访问策略等）被忽略。
				forced++
			} else if embyBool(source, "SupportsDirectPlay") {
				// 上游说可直放，客户端本会直连 Path 绕开 AetherLink。
				reclaimed++
			}
			source["SupportsDirectPlay"] = false
			source["SupportsDirectStream"] = true
			// 抹掉「为什么不能直放」的理由：已经强制接入，留着会让 PlaybackInfo
			// 自相矛盾，部分客户端仍会照它去要转码 HLS。
			source["TranscodeReasons"] = []any{}
			source["TranscodingReasons"] = []any{}
			directPlayAllowed = true
			mutated = true
		}
		p.rememberPlaybackSource(itemID, embyMediaSourceFromMap(source), directPlayAllowed)
		if !directPlayAllowed {
			reason := embyDirectPlayBlockReason(source)
			if p.claimsSources {
				// 模式要求流量经我们，判定却被卡片开关让给了上游：这一句是排障
				// 时唯一能区分「模式选错」与「开关关着」的线索。
				reason += "（卡片关着「无视上游的不可直放判定」，本次交回上游转码）"
			}
			skipped = append(skipped, reason)
			continue
		}
		source["DirectStreamUrl"] = embyDirectStreamURL(prefix, itemID, source)
		changed++
		mutated = true
	}
	if changed == 0 {
		if len(skipped) > 0 {
			logx.Infof("[%s] PlaybackInfo 保留 %d 个 STRM 媒体源由上游转码（item=%s）：%s", p.Name(), len(skipped), itemID, strings.Join(uniqueStrings(skipped), "、"))
		} else {
			logx.Infof("[%s] PlaybackInfo 未发现 STRM 媒体源（item=%s），保持上游播放能力不变", p.Name(), itemID)
		}
		if !mutated {
			return 0, nil
		}
	}

	encodedSources, err := json.Marshal(sources)
	if err != nil {
		return 0, err
	}
	envelope["MediaSources"] = encodedSources
	encodedBody, err := json.Marshal(envelope)
	if err != nil {
		return 0, err
	}
	response.Body = io.NopCloser(bytes.NewReader(encodedBody))
	response.ContentLength = int64(len(encodedBody))
	response.Header.Set("Content-Length", strconv.Itoa(len(encodedBody)))
	response.Header.Set("Content-Type", "application/json; charset=utf-8")
	response.Header.Del("Content-Encoding")
	response.Header.Del("ETag")
	response.Header.Del("Content-MD5")
	if len(skipped) > 0 {
		logx.Infof("[%s] PlaybackInfo 已让 %d 个兼容的 STRM 媒体源接入 AetherLink%s，另有 %d 个保留上游转码（item=%s）：%s", p.Name(), changed, forceNote(forced, reclaimed), len(skipped), itemID, strings.Join(uniqueStrings(skipped), "、"))
	} else if forced > 0 || reclaimed > 0 {
		// 强制接入时字节的去向由卡片模式定，日志必须写对：中继模式下写成
		// 「接入 302」会把排障方向直接带偏。
		logx.Infof("[%s] PlaybackInfo 强制让 %d 个 STRM 媒体源接入 AetherLink%s，字节由 AetherLink %s（item=%s）", p.Name(), changed, forceNote(forced, reclaimed), p.forceDelivery, itemID)
	} else {
		logx.Infof("[%s] PlaybackInfo 已让 %d 个兼容的 STRM 媒体源接入 AetherLink（item=%s），客户端下一步应请求 /Videos/%s/stream", p.Name(), changed, itemID, itemID)
	}
	return changed, nil
}

func embyAllowsDirectPlay(source map[string]any) bool {
	if value, exists := source["SupportsDirectPlay"]; exists {
		supported, _ := value.(bool)
		return supported
	}
	// 兼容极老版本：字段缺失且没有任何转码原因时，按可直放处理。
	return len(embyTranscodeReasons(source)) == 0 && strings.TrimSpace(embyString(source, "DirectStreamUrl")) != ""
}

func embyDirectPlayBlockReason(source map[string]any) string {
	reasons := embyTranscodeReasons(source)
	if len(reasons) == 0 {
		return "上游判定当前客户端不能直接播放原始文件"
	}
	labels := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		labels = append(labels, embyTranscodeReasonLabel(reason))
	}
	return strings.Join(uniqueStrings(labels), "+")
}

func embyTranscodeReasons(source map[string]any) []string {
	reasons := make([]string, 0)
	for _, key := range []string{"TranscodeReasons", "TranscodingReasons"} {
		switch value := source[key].(type) {
		case string:
			reasons = append(reasons, splitEmbyReasons(value)...)
		case []any:
			for _, entry := range value {
				if text, ok := entry.(string); ok {
					reasons = append(reasons, splitEmbyReasons(text)...)
				}
			}
		}
	}
	if transcodeURL := strings.TrimSpace(embyString(source, "TranscodingUrl")); transcodeURL != "" {
		if parsed, err := url.Parse(transcodeURL); err == nil {
			reasons = append(reasons, splitEmbyReasons(parsed.Query().Get("TranscodeReasons"))...)
		}
	}
	return uniqueStrings(reasons)
}

func splitEmbyReasons(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' })
	reasons := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			reasons = append(reasons, trimmed)
		}
	}
	return reasons
}

func embyTranscodeReasonLabel(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "videocodecnotsupported":
		return "视频编码不兼容"
	case "audiocodecnotsupported":
		return "音频编码不兼容"
	case "containernotsupported":
		return "容器格式不兼容"
	case "containerbitrateexceedslimit":
		return "码率超过客户端限制"
	case "videoresolutionnotsupported":
		return "分辨率不兼容"
	case "videobitdepthnotsupported":
		return "视频位深不兼容"
	case "videoprofilenotsupported":
		return "视频 Profile 不兼容"
	case "videolevelnotsupported":
		return "视频 Level 不兼容"
	case "audiochannelsnotsupported":
		return "音频声道不兼容"
	case "subtitlesnotsupported", "subtitlecodecnotsupported":
		return "字幕格式不兼容"
	default:
		return reason
	}
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	return unique
}

// forceNote 把强制接入下的明细写进日志，两个数都为 0 时返回空串。
// 分成两个数是因为它们的处置不同：forced 是忽略了上游的不可直放判定（原本
// 会让客户端去要转码 HLS），reclaimed 是上游说可直放（原本会让客户端直连
// Path 绕开 AetherLink）——只报一个数会让另一种情况在日志里完全消失。
func forceNote(forced, reclaimed int) string {
	parts := make([]string, 0, 2)
	if forced > 0 {
		parts = append(parts, fmt.Sprintf("其中 %d 个忽略了上游的不可直放判定", forced))
	}
	if reclaimed > 0 {
		parts = append(parts, fmt.Sprintf("%d 个由上游直放改为引回 AetherLink", reclaimed))
	}
	if len(parts) == 0 {
		return ""
	}
	return "（" + strings.Join(parts, "，") + "）"
}

func isEmbyStrmPlaybackSource(source map[string]any) bool {
	location := strings.TrimSpace(embyString(source, "Path"))
	container := strings.TrimSpace(embyString(source, "Container"))
	protocol := strings.TrimSpace(embyString(source, "Protocol"))
	return strings.EqualFold(container, "strm") ||
		strings.HasSuffix(strings.ToLower(location), ".strm") ||
		(strings.EqualFold(protocol, "Http") && isHTTPURL(location))
}

func embyDirectStreamURL(prefix, itemID string, source map[string]any) string {
	directURL := strings.TrimSpace(embyString(source, "DirectStreamUrl"))
	parsed, err := url.Parse(directURL)
	if err != nil {
		parsed = &url.URL{}
	}
	query := parsed.Query()
	if sourceID := strings.TrimSpace(embyString(source, "Id")); sourceID != "" {
		query.Set("MediaSourceId", sourceID)
	}
	query.Set("Static", "true")

	route := "Videos"
	container := embyPlaybackContainer(source)
	if strings.EqualFold(embyString(source, "MediaType"), "Audio") ||
		strings.Contains(strings.ToLower(directURL), "/audio/") || isAudioContainer(container) {
		route = "Audio"
	}
	prefix = embyDirectStreamPrefix(prefix, parsed.Path)
	streamPath := prefix + "/" + route + "/" + url.PathEscape(itemID) + "/stream"
	if container != "" {
		streamPath += "." + container
	}
	parsed.Path = streamPath
	parsed.RawPath = ""
	parsed.RawQuery = query.Encode()
	parsed.Scheme = ""
	parsed.Host = ""
	return parsed.String()
}

func embyDirectStreamPrefix(fallback, existingPath string) string {
	candidate := fallback
	lowered := strings.ToLower(existingPath)
	for _, marker := range []string{"/videos/", "/audio/"} {
		if index := strings.Index(lowered, marker); index >= 0 {
			candidate = existingPath[:index]
			break
		}
	}
	segments := strings.Split(strings.Trim(candidate, "/"), "/")
	cleaned := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		if len(cleaned) > 0 && strings.EqualFold(cleaned[len(cleaned)-1], segment) {
			continue
		}
		cleaned = append(cleaned, segment)
	}
	if len(cleaned) == 0 {
		return ""
	}
	return "/" + strings.Join(cleaned, "/")
}

func embyPlaybackContainer(source map[string]any) string {
	container := strings.ToLower(strings.TrimSpace(embyString(source, "Container")))
	if container != "" && container != "strm" && embyContainerRe.MatchString(container) {
		return container
	}
	location := strings.ReplaceAll(embyString(source, "Path"), "\\", "/")
	if parsed, err := url.Parse(location); err == nil && parsed.Path != "" {
		location = parsed.Path
	}
	extension := strings.TrimPrefix(strings.ToLower(path.Ext(location)), ".")
	if embyContainerRe.MatchString(extension) {
		return extension
	}
	return ""
}

func isAudioContainer(container string) bool {
	switch strings.ToLower(container) {
	case "m4a", "m4b", "mp3", "flac", "opus", "ogg", "oga", "aac", "wav", "wma", "webma", "mka":
		return true
	default:
		return false
	}
}

func embyString(source map[string]any, key string) string {
	value, _ := source[key].(string)
	return value
}

func embyBool(source map[string]any, key string) bool {
	value, _ := source[key].(bool)
	return value
}

func embyMediaSourceFromMap(source map[string]any) embyMediaSource {
	return embyMediaSource{
		ID:        embyString(source, "Id"),
		Path:      embyString(source, "Path"),
		Name:      embyString(source, "Name"),
		Container: embyString(source, "Container"),
		Protocol:  embyString(source, "Protocol"),
	}
}

func embyPlaybackSourceKey(itemID, sourceID string) string {
	return itemID + "\x00" + sourceID
}

func (p *embyProvider) rememberPlaybackSource(itemID string, source embyMediaSource, directPlayAllowed bool) {
	if itemID == "" || source.Path == "" {
		return
	}
	p.playbackMu.Lock()
	defer p.playbackMu.Unlock()
	if p.playbackSources == nil {
		p.playbackSources = make(map[string]cachedEmbyMediaSource)
	}
	if len(p.playbackSources) >= embyPlaybackSourceMax {
		now := time.Now()
		for key, cached := range p.playbackSources {
			if now.After(cached.expiresAt) {
				delete(p.playbackSources, key)
			}
		}
	}
	if len(p.playbackSources) >= embyPlaybackSourceMax {
		for key := range p.playbackSources {
			delete(p.playbackSources, key)
			break
		}
	}
	cached := cachedEmbyMediaSource{source: source, directPlayAllowed: directPlayAllowed, expiresAt: time.Now().Add(embyPlaybackSourceTTL)}
	p.playbackSources[embyPlaybackSourceKey(itemID, source.ID)] = cached
	if source.ID == "" {
		p.playbackSources[embyPlaybackSourceKey(itemID, "")] = cached
	}
}

func (p *embyProvider) rememberedPlaybackSource(itemID, sourceID string) (embyMediaSource, bool, bool) {
	p.playbackMu.Lock()
	defer p.playbackMu.Unlock()
	if p.playbackSources == nil {
		return embyMediaSource{}, false, false
	}
	key := embyPlaybackSourceKey(itemID, sourceID)
	cached, ok := p.playbackSources[key]
	if !ok && sourceID != "" {
		key = embyPlaybackSourceKey(itemID, "")
		cached, ok = p.playbackSources[key]
	}
	if !ok || time.Now().After(cached.expiresAt) {
		delete(p.playbackSources, key)
		return embyMediaSource{}, false, false
	}
	return cached.source, cached.directPlayAllowed, true
}

// Match intercepts Emby's direct-play and download endpoints. HLS/transcode
// segment routes are deliberately left untouched: the upstream must produce
// those itself because it needs to read the media.
func (p *embyProvider) Match(request *http.Request) (MediaRef, bool) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		return MediaRef{}, false
	}
	requestPath := path.Clean(request.URL.Path)
	mediaSourceID := request.URL.Query().Get("MediaSourceId")
	if mediaSourceID == "" {
		mediaSourceID = request.URL.Query().Get("mediaSourceId")
	}
	if matches := embyStreamRe.FindStringSubmatch(requestPath); matches != nil {
		return MediaRef{Kind: RefStream, ItemID: matches[1], MediaSourceID: mediaSourceID}, true
	}
	if matches := embyDownloadRe.FindStringSubmatch(requestPath); matches != nil {
		return MediaRef{Kind: RefStream, ItemID: matches[1], MediaSourceID: mediaSourceID}, true
	}
	return MediaRef{}, false
}

type embyMediaSource struct {
	ID           string `json:"Id"`
	Path         string `json:"Path"`
	Name         string `json:"Name"`
	Container    string `json:"Container"`
	Size         int64  `json:"Size"`
	RunTimeTicks int64  `json:"RunTimeTicks"`
	Protocol     string `json:"Protocol"`
}

type embyItem struct {
	ID           string            `json:"Id"`
	Name         string            `json:"Name"`
	Type         string            `json:"Type"`
	Path         string            `json:"Path"`
	Album        string            `json:"Album"`
	AlbumArtist  string            `json:"AlbumArtist"`
	SeriesName   string            `json:"SeriesName"`
	RunTimeTicks int64             `json:"RunTimeTicks"`
	MediaSources []embyMediaSource `json:"MediaSources"`
}

type embyItemsResponse struct {
	Items            []embyItem `json:"Items"`
	TotalRecordCount int        `json:"TotalRecordCount"`
}

// ticksToSeconds converts Emby's 100-nanosecond ticks to seconds.
func ticksToSeconds(ticks int64) float64 {
	if ticks <= 0 {
		return 0
	}
	return float64(ticks) / 10_000_000
}

// embyPlaybackInfo is the /Items/:id/PlaybackInfo response. It is the only Emby
// endpoint that is guaranteed to return MediaSources.
type embyPlaybackInfo struct {
	MediaSources []embyMediaSource `json:"MediaSources"`
}

// fetchItem loads a single item including media source paths.
//
// 两条路由都试，因为 Emby 与飞牛影视对它们的支持并不一致：
//
//   - /Items?Ids= 在 Emby 上最省事（一次调用同时给出条目元数据与媒体源），
//     但飞牛影视没有实现它 —— 请求落到单页应用上回一整页 HTML（HTTP 200），
//     json 解析必然失败，整条解析链路就此断掉，表现成「能进库、能浏览，
//     但一播放就报 invalid character '<'」。
//   - /Items/{id} 是标准单项路由，飞牛影视实现了它（参考项目 LitePan 取条目
//     详情用的正是这一条）。
//
// 所以按方言定一个优先级再依次兜底：哪条先给出带媒体源的条目就用哪条；
// 两条都没给出媒体源时再去问 PlaybackInfo。少了媒体源，Emby 系就永远只能看到
// .strm 路径，于是退化成透传 —— 用户看到的「反代通了但不 302」。
func (p *embyProvider) fetchItem(ctx context.Context, itemID string) (embyItem, error) {
	var (
		item     embyItem
		found    bool
		failures []string
	)
	for _, lookup := range p.itemLookupOrder() {
		loaded, err := p.fetchItemBy(ctx, itemID, lookup)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s 路由: %v", lookup, err))
			logx.Debugf("[%s] 取条目 %s 失败（%s 路由）: %v", p.Name(), itemID, lookup, err)
			continue
		}
		if !found {
			item, found = loaded, true
		}
		if len(loaded.MediaSources) > 0 {
			return loaded, nil
		}
	}
	if !found {
		if len(failures) > 0 {
			// 两条路由的失败原因都带上：只报最后一条，就等于把「另一条其实
			// 能用」这类情况藏起来了，而这条错误是用户唯一能看到的线索。
			return embyItem{}, fmt.Errorf("取条目 %s 失败（%s）", itemID, strings.Join(failures, "；"))
		}
		return embyItem{}, fmt.Errorf("emby item %q not found", itemID)
	}
	if sources, err := p.fetchPlaybackSources(ctx, itemID); err != nil {
		logx.Debugf("[%s] item %s PlaybackInfo 取不到媒体源: %v", p.Name(), itemID, err)
	} else {
		item.MediaSources = sources
	}
	return item, nil
}

// embyItemLookup 是取单条 Item 的路由形态。
type embyItemLookup int

const (
	// embyLookupByIDs 走集合路由 /Items?Ids=。
	embyLookupByIDs embyItemLookup = iota
	// embyLookupByID 走单项路由 /Items/{id}。
	embyLookupByID
	// embyLookupPlayback 走播放协商 /Items/{id}/PlaybackInfo。它给的是媒体源，
	// 不是条目元数据，但足以承接解析 —— 飞牛影视只剩这一条可用。
	embyLookupPlayback
)

func (l embyItemLookup) String() string {
	switch l {
	case embyLookupByID:
		return "/Items/{id}"
	case embyLookupPlayback:
		return "/Items/{id}/PlaybackInfo"
	}
	return "/Items?Ids="
}

// itemLookupOrder 按方言给出取条目的尝试顺序。
func (p *embyProvider) itemLookupOrder() []embyItemLookup {
	if p.preferItemByID {
		// 飞牛影视：两种条目路由都不实现（都回单页应用的 HTML），只有播放协商
		// 那条是真的。所以把它排在最前 —— 既是唯一确定可用的，也省掉两次注定
		// 失败的请求；条目路由留作兜底，万一将来版本补上了也能用。
		return []embyItemLookup{embyLookupPlayback, embyLookupByID, embyLookupByIDs}
	}
	// Emby 三种都支持：集合路由一次就能同时拿到元数据与媒体源，能用就不多打。
	return []embyItemLookup{embyLookupByIDs, embyLookupByID, embyLookupPlayback}
}

func (p *embyProvider) fetchItemBy(ctx context.Context, itemID string, lookup embyItemLookup) (embyItem, error) {
	if lookup == embyLookupPlayback {
		sources, err := p.fetchPlaybackSources(ctx, itemID)
		if err != nil {
			return embyItem{}, err
		}
		item := embyItem{ID: itemID, MediaSources: sources}
		// 条目本身拿不到，Path 只能从媒体源上取：调用方在没有可用媒体源时靠它
		// 退回读指针文件，缺了它这条兜底就断了。
		if len(sources) > 0 {
			item.Path = sources[0].Path
		}
		return item, nil
	}

	query := url.Values{}
	query.Set("Fields", "Path,MediaSources")
	// 带上 UserId：不少 Emby 版本只在「以某个用户身份查询」时才展开 MediaSources。
	if userID := p.resolveUserID(ctx); userID != "" {
		query.Set("UserId", userID)
	}
	if lookup == embyLookupByID {
		var item embyItem
		if err := p.client.getJSON(ctx, "/Items/"+url.PathEscape(itemID), query, &item); err != nil {
			return embyItem{}, err
		}
		if strings.TrimSpace(item.ID) == "" {
			return embyItem{}, fmt.Errorf("emby item %q not found", itemID)
		}
		return item, nil
	}
	query.Set("Ids", itemID)
	var response embyItemsResponse
	if err := p.client.getJSON(ctx, "/Items", query, &response); err != nil {
		return embyItem{}, err
	}
	if len(response.Items) == 0 {
		return embyItem{}, fmt.Errorf("emby item %q not found", itemID)
	}
	return response.Items[0], nil
}

// fetchPlaybackSources asks PlaybackInfo for the media sources of one item.
//
// Emby 的 PlaybackInfo 在多数版本上要求带 UserId，缺了会直接 400。
// 这里先取一个用户 ID（取到就缓存），拿不到再裸调一次，最大限度保证能读到
// MediaSources —— 读不到 MediaSources，Emby 侧就永远只能看到 .strm 路径，
// 于是退化成透传，也就是用户看到的「反代通了但不 302」。
func (p *embyProvider) fetchPlaybackSources(ctx context.Context, itemID string) ([]embyMediaSource, error) {
	endpoint := "/Items/" + url.PathEscape(itemID) + "/PlaybackInfo"
	var lastErr error
	for _, userID := range p.playbackUserCandidates(ctx) {
		query := url.Values{}
		if userID != "" {
			query.Set("UserId", userID)
		}
		var info embyPlaybackInfo
		if err := p.client.getJSON(ctx, endpoint, query, &info); err != nil {
			lastErr = err
			continue
		}
		if len(info.MediaSources) == 0 {
			lastErr = fmt.Errorf("emby 条目 %q 的 PlaybackInfo 没有返回媒体源", itemID)
			continue
		}
		return info.MediaSources, nil
	}
	return nil, lastErr
}

// playbackUserCandidates 返回调用 PlaybackInfo 时可用的 UserId 列表，末尾始终留
// 一个空串，表示「不带 UserId 再试一次」。
//
// 优先用「播放器自己请求里带的 UserId」与「登录换令牌时上游告知的 UserId」：这两
// 个都在手边，不必额外调一次 /Users，而且对飞牛尤其重要 —— 它的 Emby 兼容层只
// 实现了客户端真正会调的接口，/Users 未必有。两者都拿不到时才回头去问 /Users
// （结果会缓存，不会每次播放都问）。
func (p *embyProvider) playbackUserCandidates(ctx context.Context) []string {
	found := make([]string, 0, 3)
	if userID := contextClientIdentity(ctx).UserID; userID != "" {
		found = append(found, userID)
	}
	if userID := p.client.loggedInUserID(); userID != "" {
		found = append(found, userID)
	}
	if len(found) == 0 {
		if userID := p.resolveUserID(ctx); userID != "" {
			found = append(found, userID)
		}
	}

	candidates := make([]string, 0, len(found)+1)
	seen := make(map[string]bool, len(found))
	for _, userID := range found {
		if seen[userID] {
			continue
		}
		seen[userID] = true
		candidates = append(candidates, userID)
	}
	return append(candidates, "")
}

// resolveUserID 取一个可用的 Emby 用户 ID，优先管理员。结果缓存在 provider 上，
// 每次播放都去问一遍 /Users 太浪费。
func (p *embyProvider) resolveUserID(ctx context.Context) string {
	p.userMu.Lock()
	defer p.userMu.Unlock()
	if p.userLookedUp {
		return p.userID
	}
	p.userLookedUp = true

	var users []embyUser
	if err := p.client.getJSON(ctx, "/Users", nil, &users); err != nil {
		logx.Debugf("[emby] 取用户列表失败，PlaybackInfo 将不带 UserId: %v", err)
		return ""
	}
	for _, user := range users {
		if user.Policy.IsAdministrator {
			p.userID = user.ID
			return p.userID
		}
	}
	if len(users) > 0 {
		p.userID = users[0].ID
	}
	return p.userID
}

type embyUser struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Policy struct {
		IsAdministrator bool `json:"IsAdministrator"`
	} `json:"Policy"`
}

// MediaTarget resolves what the requested Emby media source really is.
//
// Emby resolves .strm pointers itself while scanning: the library item keeps the
// .strm path, but its MediaSource carries the URL from inside the pointer with
// Protocol "Http" and Container "strm". That is the whole reason Emby 302 works
// without mounting any media directory — the direct URL comes straight from the
// API. Only when Emby reports a plain filesystem path do we fall back to reading
// a pointer file ourselves.
func (p *embyProvider) MediaTarget(ctx context.Context, ref MediaRef) (MediaTarget, error) {
	if source, directPlayAllowed, ok := p.rememberedPlaybackSource(ref.ItemID, ref.MediaSourceID); ok {
		// 判定被卡片开关放行时（见 ignoresVerdict）不采纳：客户端要的就是原始
		// 文件，strm 的真实目标本来就是直链，交给 302 或中继即可。
		if !directPlayAllowed && !p.ignoresVerdict {
			return MediaTarget{}, ErrDirectPlayUnsupported
		}
		return embyTarget(source), nil
	}
	item, err := p.fetchItem(ctx, ref.ItemID)
	if err != nil {
		return MediaTarget{}, err
	}
	source, ok := selectMediaSource(item, ref.MediaSourceID)
	if !ok {
		if item.Path == "" {
			return MediaTarget{}, fmt.Errorf("emby item %q has no media source path", ref.ItemID)
		}
		return embyTarget(embyMediaSource{Path: item.Path}), nil
	}
	target := embyTarget(source)
	if target.Path == "" && target.URL == "" {
		// Some Emby builds omit Path on the media source but keep it on the item.
		return embyTarget(embyMediaSource{Path: item.Path, Container: source.Container, Protocol: source.Protocol}), nil
	}
	return target, nil
}

// selectMediaSource prefers the media source the player asked for and otherwise
// falls back to the first source that carries a location.
func selectMediaSource(item embyItem, mediaSourceID string) (embyMediaSource, bool) {
	if mediaSourceID != "" {
		for _, source := range item.MediaSources {
			if source.ID == mediaSourceID {
				return source, true
			}
		}
	}
	for _, source := range item.MediaSources {
		if source.Path != "" {
			return source, true
		}
	}
	return embyMediaSource{}, false
}

// embyTarget classifies one media source into a direct URL or a filesystem path.
func embyTarget(source embyMediaSource) MediaTarget {
	container := strings.ToLower(strings.TrimSpace(source.Container))
	location := strings.TrimSpace(source.Path)
	// Protocol Http is Emby's own marker for "this source is a remote URL",
	// which is exactly what a resolved .strm looks like. The prefix check covers
	// builds that leave Protocol empty.
	if strings.EqualFold(source.Protocol, "Http") || isHTTPURL(location) {
		return MediaTarget{URL: location, Path: location, Container: container}
	}
	return MediaTarget{Path: location, Container: container}
}

func isHTTPURL(candidate string) bool {
	lowered := strings.ToLower(candidate)
	return strings.HasPrefix(lowered, "http://") || strings.HasPrefix(lowered, "https://")
}

type embySystemInfo struct {
	ServerName string `json:"ServerName"`
	Version    string `json:"Version"`
	ID         string `json:"Id"`
}

func (p *embyProvider) Ping(ctx context.Context) (string, error) {
	var info embySystemInfo
	if err := p.client.getJSON(ctx, "/System/Info", nil, &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("emby %s v%s", info.ServerName, info.Version), nil
}

type embyVirtualFolder struct {
	Name           string `json:"Name"`
	ItemID         string `json:"ItemId"`
	CollectionType string `json:"CollectionType"`
}

func (p *embyProvider) Libraries(ctx context.Context) ([]Library, error) {
	var folders []embyVirtualFolder
	if err := p.client.getJSON(ctx, "/Library/VirtualFolders", nil, &folders); err != nil {
		return nil, err
	}
	libraries := make([]Library, 0, len(folders))
	for _, folder := range folders {
		libraries = append(libraries, Library{
			ID:        folder.ItemID,
			Name:      folder.Name,
			MediaType: folder.CollectionType,
			Provider:  "emby",
		})
	}
	return libraries, nil
}

// embyItemTypes limits browsing to leaf playable types plus containers that are
// meaningful in a STRM library.
const embyItemTypes = "Movie,Episode,Audio,AudioBook,Video,MusicVideo"

func (p *embyProvider) Items(ctx context.Context, libraryID string, limit, page int, search string) ([]Item, int, error) {
	query := url.Values{}
	query.Set("Recursive", "true")
	query.Set("IncludeItemTypes", embyItemTypes)
	query.Set("Fields", "Path,MediaSources")
	query.Set("Limit", strconv.Itoa(limit))
	query.Set("StartIndex", strconv.Itoa(limit*page))
	query.Set("SortBy", "SortName")
	if libraryID != "" {
		query.Set("ParentId", libraryID)
	}
	if search != "" {
		query.Set("SearchTerm", search)
	}
	var response embyItemsResponse
	if err := p.client.getJSON(ctx, "/Items", query, &response); err != nil {
		return nil, 0, err
	}
	items := make([]Item, 0, len(response.Items))
	for _, item := range response.Items {
		items = append(items, Item{
			ID:        item.ID,
			Title:     item.Name,
			Author:    firstNonEmpty(item.AlbumArtist, item.SeriesName, item.Album),
			LibraryID: libraryID,
			MediaType: item.Type,
			NumFiles:  len(item.MediaSources),
			NumStrm:   countStrmSources(item),
			Duration:  ticksToSeconds(item.RunTimeTicks),
		})
	}
	return items, response.TotalRecordCount, nil
}

func (p *embyProvider) ItemFiles(ctx context.Context, itemID string) (Item, []File, error) {
	item, err := p.fetchItem(ctx, itemID)
	if err != nil {
		return Item{}, nil, err
	}
	files := make([]File, 0, len(item.MediaSources))
	for index, source := range item.MediaSources {
		mediaPath := source.Path
		if mediaPath == "" {
			mediaPath = item.Path
		}
		files = append(files, File{
			ID:       source.ID,
			Index:    index,
			Filename: firstNonEmpty(source.Name, path.Base(strings.ReplaceAll(mediaPath, "\\", "/"))),
			Path:     mediaPath,
			Ext:      strings.ToLower(path.Ext(strings.ReplaceAll(mediaPath, "\\", "/"))),
			Size:     source.Size,
			Duration: ticksToSeconds(source.RunTimeTicks),
			IsStrm:   strings.HasSuffix(strings.ToLower(mediaPath), ".strm"),
		})
	}
	summary := Item{
		ID:        item.ID,
		Title:     item.Name,
		Author:    firstNonEmpty(item.AlbumArtist, item.SeriesName, item.Album),
		MediaType: item.Type,
		NumFiles:  len(files),
		NumStrm:   countStrmFiles(files),
		Duration:  ticksToSeconds(item.RunTimeTicks),
	}
	return summary, files, nil
}

// PlaybackPath uses the download endpoint because it is the only direct byte
// delivery route that does not require a device profile negotiation.
func (p *embyProvider) PlaybackPath(itemID, fileID string) string {
	target := "/emby/Items/" + url.PathEscape(itemID) + "/Download"
	if fileID != "" {
		target += "?MediaSourceId=" + url.QueryEscape(fileID)
	}
	return target
}

func countStrmSources(item embyItem) int {
	count := 0
	for _, source := range item.MediaSources {
		if strings.HasSuffix(strings.ToLower(source.Path), ".strm") {
			count++
		}
	}
	if count == 0 && strings.HasSuffix(strings.ToLower(item.Path), ".strm") {
		count = 1
	}
	return count
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

var _ Provider = (*embyProvider)(nil)
