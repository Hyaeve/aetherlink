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
}

type cachedEmbyMediaSource struct {
	source    embyMediaSource
	expiresAt time.Time
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

// RewriteResponse 只把 STRM 对应的媒体源切换到直放路由。否则 Emby 客户端常会
// 选择 /hls1/main/*.ts；HLS 分片不能跳转到完整 MKV/M4A 文件，AetherLink 也就
// 永远没有机会解析指针。
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
	for _, source := range sources {
		if !isEmbyStrmPlaybackSource(source) || embyBool(source, "IsInfiniteStream") {
			continue
		}
		source["SupportsDirectPlay"] = true
		source["SupportsDirectStream"] = true
		source["SupportsTranscoding"] = false
		delete(source, "TranscodingUrl")
		delete(source, "TranscodingSubProtocol")
		delete(source, "TranscodingContainer")
		source["DirectStreamUrl"] = embyDirectStreamURL(prefix, itemID, source)
		p.rememberPlaybackSource(itemID, embyMediaSourceFromMap(source))
		changed++
	}
	if changed == 0 {
		logx.Infof("[%s] PlaybackInfo 未发现 STRM 媒体源（item=%s），保持上游播放能力不变", p.Name(), itemID)
		return 0, nil
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
	logx.Infof("[%s] PlaybackInfo 已将 %d 个 STRM 媒体源切换为直放（item=%s），客户端下一步应请求 /Videos/%s/stream 并触发 302", p.Name(), changed, itemID, itemID)
	return changed, nil
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

func (p *embyProvider) rememberPlaybackSource(itemID string, source embyMediaSource) {
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
	cached := cachedEmbyMediaSource{source: source, expiresAt: time.Now().Add(embyPlaybackSourceTTL)}
	p.playbackSources[embyPlaybackSourceKey(itemID, source.ID)] = cached
	if source.ID == "" {
		p.playbackSources[embyPlaybackSourceKey(itemID, "")] = cached
	}
}

func (p *embyProvider) rememberedPlaybackSource(itemID, sourceID string) (embyMediaSource, bool) {
	p.playbackMu.Lock()
	defer p.playbackMu.Unlock()
	if p.playbackSources == nil {
		return embyMediaSource{}, false
	}
	key := embyPlaybackSourceKey(itemID, sourceID)
	cached, ok := p.playbackSources[key]
	if !ok && sourceID != "" {
		key = embyPlaybackSourceKey(itemID, "")
		cached, ok = p.playbackSources[key]
	}
	if !ok || time.Now().After(cached.expiresAt) {
		delete(p.playbackSources, key)
		return embyMediaSource{}, false
	}
	return cached.source, true
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
// /Items?Ids= is asked first because one call gives both the item metadata and
// its sources. Several Emby builds ignore Fields=MediaSources on that route,
// though, and an item without sources is exactly the case where AetherLink
// cannot tell a strm apart from a real file — so PlaybackInfo is used as a
// fallback. Without it Emby playback silently degrades to a pass-through, which
// is what "反代成功了但不 302" looks like from the outside.
func (p *embyProvider) fetchItem(ctx context.Context, itemID string) (embyItem, error) {
	query := url.Values{}
	query.Set("Ids", itemID)
	query.Set("Fields", "Path,MediaSources")
	// 带上 UserId：不少 Emby 版本只在「以某个用户身份查询」时才展开 MediaSources。
	if userID := p.resolveUserID(ctx); userID != "" {
		query.Set("UserId", userID)
	}
	var response embyItemsResponse
	if err := p.client.getJSON(ctx, "/Items", query, &response); err != nil {
		return embyItem{}, err
	}
	if len(response.Items) == 0 {
		return embyItem{}, fmt.Errorf("emby item %q not found", itemID)
	}
	item := response.Items[0]
	if len(item.MediaSources) == 0 {
		if sources, err := p.fetchPlaybackSources(ctx, itemID); err != nil {
			logx.Debugf("[emby] item %s PlaybackInfo 取不到媒体源: %v", itemID, err)
		} else {
			item.MediaSources = sources
		}
	}
	return item, nil
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

// playbackUserCandidates 返回调用 PlaybackInfo 时可用的 UserId 列表，
// 末尾始终留一个空串，表示「不带 UserId 再试一次」。
func (p *embyProvider) playbackUserCandidates(ctx context.Context) []string {
	if userID := p.resolveUserID(ctx); userID != "" {
		return []string{userID, ""}
	}
	return []string{""}
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
	if source, ok := p.rememberedPlaybackSource(ref.ItemID, ref.MediaSourceID); ok {
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
