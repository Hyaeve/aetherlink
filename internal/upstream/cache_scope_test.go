package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aetherlink/aetherlink/internal/config"
)

func playbackContext(request *http.Request) context.Context {
	return WithUserAgent(WithClientCredentials(context.Background(), request), request.UserAgent())
}

func TestPlaybackSourceCacheIsolatesIdentityAndUserAgent(t *testing.T) {
	provider := &embyProvider{}
	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.Header.Set("X-Emby-Token", "user-a")
	first.Header.Set("User-Agent", "Player/A")
	provider.rememberPlaybackSource("movie", embyMediaSource{ID: "source", Path: "https://cdn.example/a"}, playbackContext(first))
	other := httptest.NewRequest(http.MethodGet, "/", nil)
	other.Header.Set("X-Emby-Token", "user-a")
	other.Header.Set("User-Agent", "Player/B")
	if _, ok := provider.rememberedPlaybackSource("movie", "source", playbackContext(other)); ok {
		t.Fatal("different UA reused playback source")
	}
	other.Header.Set("X-Emby-Token", "user-b")
	other.Header.Set("User-Agent", "Player/A")
	if _, ok := provider.rememberedPlaybackSource("movie", "source", playbackContext(other)); ok {
		t.Fatal("different identity reused playback source")
	}
	if source, ok := provider.rememberedPlaybackSource("movie", "source", playbackContext(first)); !ok || source.Path != "https://cdn.example/a" {
		t.Fatalf("original source not retained: %+v %v", source, ok)
	}
}

func TestProviderCacheNamespaceTracksMediaConfiguration(t *testing.T) {
	original := config.Upstream{Name: "same", Type: config.UpstreamFnos, BaseURL: "http://example.test", Username: "user", Password: "secret"}
	namespace := func(cfg config.Upstream) string {
		provider, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return provider.(interface{ CacheNamespace() string }).CacheNamespace()
	}
	first := namespace(original)
	for _, mutate := range []func(*config.Upstream){
		func(cfg *config.Upstream) { cfg.BaseURL = "http://other.test" },
		func(cfg *config.Upstream) { cfg.Password = "other-secret" },
		func(cfg *config.Upstream) { cfg.APIKey = "other-key" },
		func(cfg *config.Upstream) { cfg.Type = config.UpstreamEmby },
		func(cfg *config.Upstream) { cfg.StrmRoots = []string{"/new-root"} },
	} {
		changed := original
		mutate(&changed)
		if namespace(changed) == first {
			t.Fatal("media configuration change reused old namespace")
		}
	}
	original.RedirectMode = config.RedirectPublic
	original.ListenPort = 9000
	original.Enabled = config.Bool(false)
	if namespace(original) != first {
		t.Fatal("delivery-only change unnecessarily invalidated media cache")
	}
}

func TestPlaybackIdentitySupportsAuthorizationAndCookies(t *testing.T) {
	for _, authorization := range []string{`MediaBrowser Client="Player", Token="valid-token"`, "Bearer valid-token"} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Authorization", authorization)
		if token := contextClientToken(playbackContext(request)); token != "valid-token" {
			t.Fatalf("token=%q", token)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Cookie", "session=first-user")
	first := PlaybackCacheScope(playbackContext(request))
	request.Header.Set("Cookie", "session=second-user")
	if PlaybackCacheScope(playbackContext(request)) == first {
		t.Fatal("cookie-authenticated users shared playback scope")
	}
}
