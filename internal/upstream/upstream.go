// Package upstream wraps the media servers AetherLink reverse proxies.
//
// A provider does three things: it recognises which proxied requests are media
// deliveries worth intercepting, it resolves the upstream-side filesystem path
// of the media behind such a request using the upstream API key, and it exposes
// enough library browsing for the AetherLink UI to show a STRM library.
package upstream

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/pathmap"
)

// RefKind labels the shape of an intercepted media request.
type RefKind string

const (
	RefLibraryFile  RefKind = "library-file"
	RefSessionTrack RefKind = "session-track"
	RefStream       RefKind = "stream"
)

// MediaRef identifies the media behind an intercepted request.
type MediaRef struct {
	Kind          RefKind
	ItemID        string
	FileID        string
	MediaSourceID string
	SessionID     string
	TrackIndex    string
}

// CacheKey is a stable identity for the resolved-target cache.
func (r MediaRef) CacheKey(upstreamName string) string {
	return strings.Join([]string{upstreamName, string(r.Kind), r.ItemID, r.FileID, r.MediaSourceID, r.SessionID, r.TrackIndex}, "|")
}

// Library is a browsable upstream library.
type Library struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Provider  string `json:"provider"`
}

// Item is one book, audiobook or video in a library.
type Item struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Author    string  `json:"author,omitempty"`
	LibraryID string  `json:"libraryId,omitempty"`
	MediaType string  `json:"mediaType,omitempty"`
	NumFiles  int     `json:"numFiles"`
	NumStrm   int     `json:"numStrm"`
	Duration  float64 `json:"duration,omitempty"`
	CoverURL  string  `json:"coverUrl,omitempty"`
}

// File is one playable file inside an item.
type File struct {
	ID       string  `json:"id"`
	Index    int     `json:"index"`
	Filename string  `json:"filename"`
	Path     string  `json:"path"`
	Ext      string  `json:"ext"`
	Size     int64   `json:"size"`
	Duration float64 `json:"duration,omitempty"`
	IsStrm   bool    `json:"isStrm"`
}

// Provider is implemented by each supported media server dialect.
type Provider interface {
	Name() string
	Type() config.UpstreamType
	// ListenPort is the AetherLink container port this upstream is served on.
	ListenPort() int
	BaseURL() *url.URL
	Mapper() *pathmap.Mapper
	Transport() http.RoundTripper
	HasCredentials() bool

	// Match reports whether the request should be intercepted for STRM
	// resolution rather than proxied verbatim.
	Match(request *http.Request) (MediaRef, bool)
	// MediaPath resolves the upstream-side path of the referenced media.
	MediaPath(ctx context.Context, ref MediaRef) (string, error)

	Ping(ctx context.Context) (string, error)
	Libraries(ctx context.Context) ([]Library, error)
	Items(ctx context.Context, libraryID string, limit, page int, search string) ([]Item, int, error)
	ItemFiles(ctx context.Context, itemID string) (Item, []File, error)
	// PlaybackPath returns the upstream path used to stream a specific file, so
	// the UI can build a direct AetherLink play URL.
	PlaybackPath(itemID, fileID string) string
}

// New builds the provider matching the configured upstream type.
func New(cfg config.Upstream) (Provider, error) {
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: invalid base_url: %w", cfg.Name, err)
	}
	rules := make([]pathmap.Rule, 0, len(cfg.PathMappings))
	for _, mapping := range cfg.PathMappings {
		rules = append(rules, pathmap.Rule{From: mapping.From, To: mapping.To})
	}
	client := &apiClient{
		base:   base,
		apiKey: strings.TrimSpace(cfg.APIKey),
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: newTransport(cfg.Insecure),
		},
	}
	shared := providerBase{
		name:   cfg.Name,
		kind:   cfg.Type,
		port:   cfg.ListenPort,
		base:   base,
		mapper: pathmap.New(rules, cfg.StrmRoots),
		client: client,
	}
	switch cfg.Type {
	case config.UpstreamAudiobookshelf:
		client.authHeader = "Authorization"
		return &absProvider{providerBase: shared}, nil
	case config.UpstreamEmby:
		client.authHeader = "X-Emby-Token"
		client.authQuery = "api_key"
		return &embyProvider{providerBase: shared}, nil
	default:
		return nil, fmt.Errorf("upstream %s: unsupported type %q", cfg.Name, cfg.Type)
	}
}

func newTransport(insecure bool) *http.Transport {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Response header timeouts are intentionally unset: cloud drive backends
		// can take tens of seconds to sign a large media URL.
	}
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return transport
}

// providerBase holds the fields shared by every provider implementation.
type providerBase struct {
	name   string
	kind   config.UpstreamType
	port   int
	base   *url.URL
	mapper *pathmap.Mapper
	client *apiClient
}

func (b *providerBase) Name() string                 { return b.name }
func (b *providerBase) Type() config.UpstreamType    { return b.kind }
func (b *providerBase) ListenPort() int              { return b.port }
func (b *providerBase) BaseURL() *url.URL            { return b.base }
func (b *providerBase) Mapper() *pathmap.Mapper      { return b.mapper }
func (b *providerBase) Transport() http.RoundTripper { return b.client.http.Transport }
func (b *providerBase) HasCredentials() bool         { return b.client.apiKey != "" }

// apiClient performs authenticated JSON calls against the upstream API.
type apiClient struct {
	base   *url.URL
	apiKey string
	http   *http.Client
	// authHeader and authQuery let the same client speak both the
	// Audiobookshelf (bearer token) and Emby (api_key) dialects.
	authHeader string
	authQuery  string
}

// ErrNoAPIKey is returned when an upstream has no API key configured, which
// means AetherLink cannot resolve media paths for it.
var ErrNoAPIKey = fmt.Errorf("upstream api key is not configured")

// getJSON issues an authenticated GET and decodes the JSON body into out.
func (c *apiClient) getJSON(ctx context.Context, endpoint string, query url.Values, out any) error {
	if c.apiKey == "" {
		return ErrNoAPIKey
	}
	target := *c.base
	target.Path = strings.TrimRight(target.Path, "/") + endpoint
	if query == nil {
		query = url.Values{}
	}
	if c.authQuery != "" {
		query.Set(c.authQuery, c.apiKey)
	}
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if c.authHeader != "" {
		request.Header.Set(c.authHeader, c.authValue())
	}

	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("GET %s returned %d: %s", endpoint, response.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}

func (c *apiClient) authValue() string {
	if c.authHeader == "Authorization" {
		return "Bearer " + c.apiKey
	}
	return c.apiKey
}
