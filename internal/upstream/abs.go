package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// absProvider speaks the Audiobookshelf API. Authentication uses an API key
// created in the Audiobookshelf UI, which is a JWT accepted as a bearer token
// on every /api route, so AetherLink acts with that key owner's permissions.
type absProvider struct {
	providerBase
}

// 两条正则都允许前面多一段路径前缀：Audiobookshelf 支持 ROUTER_BASE_PATH，
// 装在子路径下时客户端请求的是 /audiobookshelf/api/items/...，只按 ^/api/
// 匹配就会整条漏掉，表现正是「反代通了但完全不 302、日志里什么都没有」。
var (
	// [/前缀]/api/items/:itemId/file/:fileId[/download]
	absLibraryFileRe = regexp.MustCompile(`(?i)^(?:/[^/]+)?/api/items/([^/]+)/file/([^/]+)(/download)?$`)
	// [/前缀]/public/session/:sessionId/track/:index
	absSessionTrackRe = regexp.MustCompile(`(?i)^(?:/[^/]+)?/public/session/([^/]+)/track/([^/]+)$`)
)

// Match intercepts the two Audiobookshelf endpoints that deliver audio bytes.
// Everything else (UI, metadata, progress sync, covers, HLS) is proxied as-is.
func (p *absProvider) Match(request *http.Request) (MediaRef, bool) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		return MediaRef{}, false
	}
	requestPath := path.Clean(request.URL.Path)
	if matches := absLibraryFileRe.FindStringSubmatch(requestPath); matches != nil {
		return MediaRef{Kind: RefLibraryFile, ItemID: matches[1], FileID: matches[2]}, true
	}
	if matches := absSessionTrackRe.FindStringSubmatch(requestPath); matches != nil {
		return MediaRef{Kind: RefSessionTrack, SessionID: matches[1], TrackIndex: matches[2]}, true
	}
	return MediaRef{}, false
}

// absFileMetadata mirrors the shared FileMetadata object in Audiobookshelf.
type absFileMetadata struct {
	Filename string  `json:"filename"`
	Ext      string  `json:"ext"`
	Path     string  `json:"path"`
	RelPath  string  `json:"relPath"`
	Size     int64   `json:"size"`
	MtimeMs  float64 `json:"mtimeMs"`
}

type absLibraryFile struct {
	Ino      string          `json:"ino"`
	Metadata absFileMetadata `json:"metadata"`
	FileType string          `json:"fileType"`
}

type absAudioFile struct {
	Index    int             `json:"index"`
	Ino      string          `json:"ino"`
	Metadata absFileMetadata `json:"metadata"`
	Duration float64         `json:"duration"`
	Codec    string          `json:"codec"`
}

type absLibraryItem struct {
	ID           string           `json:"id"`
	LibraryID    string           `json:"libraryId"`
	MediaType    string           `json:"mediaType"`
	Path         string           `json:"path"`
	RelPath      string           `json:"relPath"`
	LibraryFiles []absLibraryFile `json:"libraryFiles"`
	Media        struct {
		Duration   float64        `json:"duration"`
		AudioFiles []absAudioFile `json:"audioFiles"`
		// Episodes 是播客单集，展开后的条目里字段名就叫 episodes。
		Episodes []absPodcastEpisode `json:"episodes"`
		Metadata struct {
			Title      string `json:"title"`
			AuthorName string `json:"authorName"`
		} `json:"metadata"`
	} `json:"media"`
}

// absPodcastEpisode 是播客单集，音频文件挂在 audioFile 上而不是 audioFiles 数组里。
type absPodcastEpisode struct {
	ID        string       `json:"id"`
	Index     int          `json:"index"`
	Title     string       `json:"title"`
	AudioFile absAudioFile `json:"audioFile"`
}

func (i absLibraryItem) title() string {
	if i.Media.Metadata.Title != "" {
		return i.Media.Metadata.Title
	}
	return path.Base(i.RelPath)
}

// MediaTarget resolves the upstream filesystem path for an intercepted request.
//
// Audiobookshelf keeps .strm files as-is and proxies them at playback time, so
// its API only ever reports the pointer path. Reading the pointer therefore
// requires AetherLink to see the same media directory.
func (p *absProvider) MediaTarget(ctx context.Context, ref MediaRef) (MediaTarget, error) {
	var (
		mediaPath string
		err       error
	)
	switch ref.Kind {
	case RefLibraryFile:
		mediaPath, err = p.libraryFilePath(ctx, ref.ItemID, ref.FileID)
	case RefSessionTrack:
		mediaPath, err = p.sessionTrackPath(ctx, ref.SessionID, ref.TrackIndex)
	default:
		return MediaTarget{}, fmt.Errorf("unsupported audiobookshelf media ref %q", ref.Kind)
	}
	if err != nil {
		return MediaTarget{}, err
	}
	return MediaTarget{Path: mediaPath, Container: strings.TrimPrefix(strings.ToLower(path.Ext(mediaPath)), ".")}, nil
}

func (p *absProvider) libraryFilePath(ctx context.Context, itemID, fileID string) (string, error) {
	var item absLibraryItem
	query := url.Values{"expanded": []string{"1"}}
	if err := p.client.getJSON(ctx, "/api/items/"+url.PathEscape(itemID), query, &item); err != nil {
		return "", err
	}
	for _, file := range item.LibraryFiles {
		if file.Ino == fileID {
			return file.Metadata.Path, nil
		}
	}
	// Newer clients may reference an audio file that is not listed as a library
	// file (for example after a partial rescan), so check audio files too.
	for _, audioFile := range item.Media.AudioFiles {
		if audioFile.Ino == fileID {
			return audioFile.Metadata.Path, nil
		}
	}
	return "", fmt.Errorf("file %q not found in audiobookshelf item %q", fileID, itemID)
}

type absSessionResponse struct {
	ID            string `json:"id"`
	LibraryItemID string `json:"libraryItemId"`
	EpisodeID     string `json:"episodeId"`
	AudioTracks   []struct {
		Index      int             `json:"index"`
		ContentURL string          `json:"contentUrl"`
		Metadata   absFileMetadata `json:"metadata"`
	} `json:"audioTracks"`
}

// sessionTrackPath resolves /public/session/:id/track/:index，也就是
// Audiobookshelf 网页端与 App 实际请求音频字节的那个入口。
//
// 这里必须分两步走：/api/session/:id 是从数据库重建会话的，返回体里
// **没有 audioTracks**（PlaybackSession 模型不持久化音轨），只靠它永远都找不到
// 音轨，表现就是 ABS 侧完全不 302。所以拿到 libraryItemId 之后再查一次条目，
// 按音轨序号定位到具体的音频文件。会话响应里恰好带了音轨时走快路径。
func (p *absProvider) sessionTrackPath(ctx context.Context, sessionID, trackIndex string) (string, error) {
	index, err := strconv.Atoi(trackIndex)
	if err != nil {
		return "", fmt.Errorf("音轨序号 %q 不是数字", trackIndex)
	}
	session, err := p.lookupSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	for _, track := range session.AudioTracks {
		if track.Index == index && track.Metadata.Path != "" {
			return track.Metadata.Path, nil
		}
	}
	// 会话响应没带音轨（常态）：回到条目本身按序号找。
	if session.LibraryItemID == "" {
		return "", fmt.Errorf("会话 %q 里既没有音轨，也没有 libraryItemId", sessionID)
	}
	return p.itemTrackPath(ctx, session.LibraryItemID, session.EpisodeID, index)
}

// lookupSession 找一个正在播放的会话。
//
// /api/session/:id 读的是数据库，而 Audiobookshelf 只在同步或关闭会话时才落库，
// 刚开始播放的会话在库里还不存在，这条接口会 404。所以拿不到就再查一次
// /api/sessions/open（内存里的活跃会话）。少了这个回退，播放刚开始的那几秒
// 全都解析失败，正好是用户最容易看到「不 302」的时刻。
func (p *absProvider) lookupSession(ctx context.Context, sessionID string) (absSessionResponse, error) {
	var session absSessionResponse
	dbErr := p.client.getJSON(ctx, "/api/session/"+url.PathEscape(sessionID), nil, &session)
	if dbErr == nil && (session.LibraryItemID != "" || len(session.AudioTracks) > 0) {
		return session, nil
	}

	var open absOpenSessionsResponse
	if err := p.client.getJSON(ctx, "/api/sessions/open", nil, &open); err != nil {
		if dbErr != nil {
			return absSessionResponse{}, fmt.Errorf("查会话 %q 失败：%v；活跃会话列表也读不到：%w", sessionID, dbErr, err)
		}
		return absSessionResponse{}, err
	}
	for _, candidate := range open.Sessions {
		if candidate.ID == sessionID {
			return candidate, nil
		}
	}
	if dbErr != nil {
		return absSessionResponse{}, fmt.Errorf("找不到会话 %q：%w", sessionID, dbErr)
	}
	return session, nil
}

type absOpenSessionsResponse struct {
	Sessions []absSessionResponse `json:"sessions"`
}

// itemTrackPath 按音轨序号在条目里定位音频文件路径。
//
// 播客单集的音轨只有一条，且 v2.21.0 之前 index 为 null、客户端会传 0，
// 所以单集分支不看序号，直接取该集的音频文件。
func (p *absProvider) itemTrackPath(ctx context.Context, itemID, episodeID string, index int) (string, error) {
	var item absLibraryItem
	query := url.Values{"expanded": []string{"1"}}
	if err := p.client.getJSON(ctx, "/api/items/"+url.PathEscape(itemID), query, &item); err != nil {
		return "", err
	}

	if episodeID != "" {
		for _, episode := range item.Media.Episodes {
			if episode.ID == episodeID {
				if episode.AudioFile.Metadata.Path == "" {
					return "", fmt.Errorf("播客单集 %q 没有音频文件路径", episodeID)
				}
				return episode.AudioFile.Metadata.Path, nil
			}
		}
		return "", fmt.Errorf("条目 %q 里找不到播客单集 %q", itemID, episodeID)
	}

	for _, audioFile := range item.Media.AudioFiles {
		if audioFile.Index == index && audioFile.Metadata.Path != "" {
			return audioFile.Metadata.Path, nil
		}
	}
	// 单文件有声书的音轨序号可能是 0 或 1，两边对不齐时退回第一条音频。
	if index <= 1 && len(item.Media.AudioFiles) == 1 {
		return item.Media.AudioFiles[0].Metadata.Path, nil
	}
	return "", fmt.Errorf("条目 %q 里找不到序号为 %d 的音轨", itemID, index)
}

type absPingResponse struct {
	Success bool `json:"success"`
}

type absStatusResponse struct {
	ServerVersion string `json:"serverVersion"`
	IsInit        bool   `json:"isInit"`
}

// Ping verifies the API key by calling an authenticated endpoint.
func (p *absProvider) Ping(ctx context.Context) (string, error) {
	var me struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Type     string `json:"type"`
	}
	if err := p.client.getJSON(ctx, "/api/me", nil, &me); err != nil {
		return "", err
	}
	var status absStatusResponse
	version := ""
	if err := p.client.getJSON(ctx, "/status", nil, &status); err == nil {
		version = status.ServerVersion
	}
	label := fmt.Sprintf("audiobookshelf user %s (%s)", me.Username, me.Type)
	if version != "" {
		label += " v" + version
	}
	return label, nil
}

type absLibrariesResponse struct {
	Libraries []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		MediaType string `json:"mediaType"`
		Provider  string `json:"provider"`
	} `json:"libraries"`
}

func (p *absProvider) Libraries(ctx context.Context) ([]Library, error) {
	var response absLibrariesResponse
	if err := p.client.getJSON(ctx, "/api/libraries", nil, &response); err != nil {
		return nil, err
	}
	libraries := make([]Library, 0, len(response.Libraries))
	for _, library := range response.Libraries {
		libraries = append(libraries, Library{ID: library.ID, Name: library.Name, MediaType: library.MediaType, Provider: library.Provider})
	}
	return libraries, nil
}

type absItemsResponse struct {
	Results []absLibraryItem `json:"results"`
	Total   int              `json:"total"`
	Page    int              `json:"page"`
}

func (p *absProvider) Items(ctx context.Context, libraryID string, limit, page int, search string) ([]Item, int, error) {
	if libraryID == "" {
		return nil, 0, fmt.Errorf("libraryId is required")
	}
	// Search uses the dedicated endpoint because the items filter parameter only
	// accepts pre-encoded filter groups, not free text.
	if search != "" {
		return p.searchItems(ctx, libraryID, limit, search)
	}
	query := url.Values{}
	query.Set("limit", strconv.Itoa(limit))
	query.Set("page", strconv.Itoa(page))
	// minified=0 keeps audioFiles in the response so STRM tracks can be counted.
	query.Set("minified", "0")
	var response absItemsResponse
	if err := p.client.getJSON(ctx, "/api/libraries/"+url.PathEscape(libraryID)+"/items", query, &response); err != nil {
		return nil, 0, err
	}
	items := make([]Item, 0, len(response.Results))
	for _, result := range response.Results {
		items = append(items, Item{
			ID:        result.ID,
			Title:     result.title(),
			Author:    result.Media.Metadata.AuthorName,
			LibraryID: result.LibraryID,
			MediaType: result.MediaType,
			NumFiles:  len(result.Media.AudioFiles),
			NumStrm:   countStrm(result.Media.AudioFiles),
			Duration:  result.Media.Duration,
		})
	}
	return items, response.Total, nil
}

func (p *absProvider) ItemFiles(ctx context.Context, itemID string) (Item, []File, error) {
	var item absLibraryItem
	query := url.Values{"expanded": []string{"1"}}
	if err := p.client.getJSON(ctx, "/api/items/"+url.PathEscape(itemID), query, &item); err != nil {
		return Item{}, nil, err
	}

	// Audio files carry the playable index and duration; library files carry the
	// inode used in the streaming URL. Audio files already include the inode so
	// they are the primary source, with library files filling in .strm entries
	// that were not registered as audio tracks.
	files := make([]File, 0, len(item.Media.AudioFiles))
	seen := map[string]bool{}
	for _, audioFile := range item.Media.AudioFiles {
		seen[audioFile.Ino] = true
		files = append(files, File{
			ID:       audioFile.Ino,
			Index:    audioFile.Index,
			Filename: audioFile.Metadata.Filename,
			Path:     audioFile.Metadata.Path,
			Ext:      audioFile.Metadata.Ext,
			Size:     audioFile.Metadata.Size,
			Duration: audioFile.Duration,
			IsStrm:   strings.EqualFold(audioFile.Metadata.Ext, ".strm"),
		})
	}
	for _, libraryFile := range item.LibraryFiles {
		if seen[libraryFile.Ino] || !strings.EqualFold(libraryFile.Metadata.Ext, ".strm") {
			continue
		}
		files = append(files, File{
			ID:       libraryFile.Ino,
			Filename: libraryFile.Metadata.Filename,
			Path:     libraryFile.Metadata.Path,
			Ext:      libraryFile.Metadata.Ext,
			Size:     libraryFile.Metadata.Size,
			IsStrm:   true,
		})
	}

	summary := Item{
		ID:        item.ID,
		Title:     item.title(),
		Author:    item.Media.Metadata.AuthorName,
		LibraryID: item.LibraryID,
		MediaType: item.MediaType,
		NumFiles:  len(files),
		NumStrm:   countStrmFiles(files),
		Duration:  item.Media.Duration,
	}
	return summary, files, nil
}

func (p *absProvider) PlaybackPath(itemID, fileID string) string {
	return "/api/items/" + url.PathEscape(itemID) + "/file/" + url.PathEscape(fileID)
}

type absSearchResponse struct {
	Book []struct {
		LibraryItem absLibraryItem `json:"libraryItem"`
	} `json:"book"`
	Podcast []struct {
		LibraryItem absLibraryItem `json:"libraryItem"`
	} `json:"podcast"`
}

// searchItems queries /api/libraries/:id/search, which returns grouped matches.
func (p *absProvider) searchItems(ctx context.Context, libraryID string, limit int, search string) ([]Item, int, error) {
	query := url.Values{}
	query.Set("q", search)
	query.Set("limit", strconv.Itoa(limit))
	var response absSearchResponse
	if err := p.client.getJSON(ctx, "/api/libraries/"+url.PathEscape(libraryID)+"/search", query, &response); err != nil {
		return nil, 0, err
	}
	matches := append(response.Book, response.Podcast...)
	items := make([]Item, 0, len(matches))
	for _, match := range matches {
		result := match.LibraryItem
		items = append(items, Item{
			ID:        result.ID,
			Title:     result.title(),
			Author:    result.Media.Metadata.AuthorName,
			LibraryID: result.LibraryID,
			MediaType: result.MediaType,
			NumFiles:  len(result.Media.AudioFiles),
			NumStrm:   countStrm(result.Media.AudioFiles),
			Duration:  result.Media.Duration,
		})
	}
	return items, len(items), nil
}

var _ Provider = (*absProvider)(nil)

func countStrm(audioFiles []absAudioFile) int {
	count := 0
	for _, audioFile := range audioFiles {
		if strings.EqualFold(audioFile.Metadata.Ext, ".strm") {
			count++
		}
	}
	return count
}

func countStrmFiles(files []File) int {
	count := 0
	for _, file := range files {
		if file.IsStrm {
			count++
		}
	}
	return count
}
