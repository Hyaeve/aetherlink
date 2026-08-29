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

var (
	// /api/items/:itemId/file/:fileId[/download]
	absLibraryFileRe = regexp.MustCompile(`^/api/items/([^/]+)/file/([^/]+)(/download)?$`)
	// /public/session/:sessionId/track/:index
	absSessionTrackRe = regexp.MustCompile(`^/public/session/([^/]+)/track/([^/]+)$`)
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
		Metadata   struct {
			Title      string `json:"title"`
			AuthorName string `json:"authorName"`
		} `json:"metadata"`
	} `json:"media"`
}

func (i absLibraryItem) title() string {
	if i.Media.Metadata.Title != "" {
		return i.Media.Metadata.Title
	}
	return path.Base(i.RelPath)
}

// MediaPath resolves the upstream filesystem path for an intercepted request.
func (p *absProvider) MediaPath(ctx context.Context, ref MediaRef) (string, error) {
	switch ref.Kind {
	case RefLibraryFile:
		return p.libraryFilePath(ctx, ref.ItemID, ref.FileID)
	case RefSessionTrack:
		return p.sessionTrackPath(ctx, ref.SessionID, ref.TrackIndex)
	default:
		return "", fmt.Errorf("unsupported audiobookshelf media ref %q", ref.Kind)
	}
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
	AudioTracks   []struct {
		Index      int             `json:"index"`
		ContentURL string          `json:"contentUrl"`
		Metadata   absFileMetadata `json:"metadata"`
	} `json:"audioTracks"`
}

// sessionTrackPath resolves /public/session/:id/track/:index. The open session
// endpoint is admin-or-owner protected, so the configured API key must belong to
// an admin user for session playback to be intercepted; when it is not, the
// caller falls back to plain proxying.
func (p *absProvider) sessionTrackPath(ctx context.Context, sessionID, trackIndex string) (string, error) {
	index, err := strconv.Atoi(trackIndex)
	if err != nil {
		return "", fmt.Errorf("invalid audio track index %q", trackIndex)
	}
	var session absSessionResponse
	if err := p.client.getJSON(ctx, "/api/session/"+url.PathEscape(sessionID), nil, &session); err != nil {
		return "", err
	}
	for _, track := range session.AudioTracks {
		if track.Index == index {
			return track.Metadata.Path, nil
		}
	}
	// Podcast episodes created before v2.21.0 have a null index and clients send 0.
	if index == 0 && len(session.AudioTracks) > 0 {
		return session.AudioTracks[0].Metadata.Path, nil
	}
	return "", fmt.Errorf("audio track %d not found in session %q", index, sessionID)
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
