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

// embyProvider speaks the Emby (and Jellyfin-compatible) HTTP API using an
// API key issued in the Emby dashboard. The key is sent as X-Emby-Token and as
// the api_key query parameter, which is what Emby accepts for admin-level calls.
type embyProvider struct {
	providerBase
}

var (
	// /Videos/:id/stream(.ext), /Audio/:id/stream(.ext), /Videos/:id/original.ext
	embyStreamRe = regexp.MustCompile(`(?i)^/(?:emby/)?(?:videos|audio)/([^/]+)/(?:stream|original|universal)(?:\.[A-Za-z0-9]+)?$`)
	// /Items/:id/Download
	embyDownloadRe = regexp.MustCompile(`(?i)^/(?:emby/)?items/([^/]+)/download$`)
)

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

// fetchItem loads a single item including media source paths.
func (p *embyProvider) fetchItem(ctx context.Context, itemID string) (embyItem, error) {
	query := url.Values{}
	query.Set("Ids", itemID)
	query.Set("Fields", "Path,MediaSources")
	var response embyItemsResponse
	if err := p.client.getJSON(ctx, "/Items", query, &response); err != nil {
		return embyItem{}, err
	}
	if len(response.Items) == 0 {
		return embyItem{}, fmt.Errorf("emby item %q not found", itemID)
	}
	return response.Items[0], nil
}

// MediaPath resolves the upstream path of the requested media source.
func (p *embyProvider) MediaPath(ctx context.Context, ref MediaRef) (string, error) {
	item, err := p.fetchItem(ctx, ref.ItemID)
	if err != nil {
		return "", err
	}
	if ref.MediaSourceID != "" {
		for _, source := range item.MediaSources {
			if source.ID == ref.MediaSourceID && source.Path != "" {
				return source.Path, nil
			}
		}
	}
	for _, source := range item.MediaSources {
		if source.Path != "" {
			return source.Path, nil
		}
	}
	if item.Path != "" {
		return item.Path, nil
	}
	return "", fmt.Errorf("emby item %q has no media source path", ref.ItemID)
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
