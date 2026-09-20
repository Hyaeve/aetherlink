package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aetherlink/aetherlink/internal/config"
)

func TestEmbyFamilyCachePlaybackDoesNotValidatePermissions(t *testing.T) {
	for _, kind := range []config.UpstreamType{config.UpstreamEmby, config.UpstreamFnos} {
		t.Run(string(kind), func(t *testing.T) {
			var upstreamCalls atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				upstreamCalls.Add(1)
				if strings.HasSuffix(request.URL.Path, "/PlaybackInfo") {
					writeJSON(t, writer, map[string]any{"MediaSources": []map[string]any{{
						"Id": "source", "Path": "https://cdn.example/movie.mkv", "Protocol": "Http", "Container": "strm",
					}}})
					return
				}
				writer.WriteHeader(http.StatusUnauthorized)
			}))
			defer origin.Close()
			factory := newEmbyTestServer
			if kind == config.UpstreamFnos {
				factory = newFnosTestServer
			}
			server, collector := factory(t, origin.URL, defaultRedirect())
			playback := httptest.NewRequest(http.MethodPost, "/Items/movie/PlaybackInfo", nil)
			playback.Header.Set("User-Agent", "Player/A")
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, playback)
			var info struct {
				MediaSources []struct {
					DirectStreamURL string `json:"DirectStreamUrl"`
				}
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &info); err != nil {
				t.Fatal(err)
			}
			if len(info.MediaSources) != 1 {
				t.Fatal("missing media source")
			}
			stream := info.MediaSources[0].DirectStreamURL
			if strings.Contains(stream, "aetherlink_ticket") {
				t.Fatal("stream URL still contains a playback ticket")
			}
			for index := 0; index < 2; index++ {
				request := httptest.NewRequest(http.MethodGet, stream, nil)
				request.Header.Set("User-Agent", "Player/A")
				result := httptest.NewRecorder()
				server.ServeHTTP(result, request)
				if result.Code != http.StatusFound {
					t.Fatalf("status=%d, want 302 without playback credentials", result.Code)
				}
			}
			if calls := upstreamCalls.Load(); calls != 1 {
				t.Fatalf("unexpected upstream permission/lookup requests: %d", calls)
			}
			if snapshot := collector.Snapshot(10); snapshot.Redirects != 2 || snapshot.CacheHits != 1 {
				t.Fatalf("unexpected cache/redirect totals: %+v", snapshot)
			}
		})
	}
}
