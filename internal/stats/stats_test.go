package stats

import (
	"path/filepath"
	"testing"
)

func TestPersistentCollectorRestoresPlaybackStatistics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "playback-stats.json")
	first := NewWithPersistence(50, path)
	first.Record(Event{Upstream: "emby", Kind: "openlist", Outcome: OutcomeRedirect})
	first.Record(Event{Upstream: "abs", Kind: "openlist", Outcome: OutcomeTranscode, CacheHit: true})
	for index := 0; index < 60; index++ {
		first.Record(Event{Upstream: "emby", Outcome: OutcomePassthrough})
	}

	second := NewWithPersistence(50, path)
	snapshot := second.Snapshot(50)
	if snapshot.TotalRequests != 62 || snapshot.Redirects != 1 || snapshot.Transcodes != 1 {
		t.Fatalf("restored snapshot = %+v", snapshot)
	}
	if snapshot.CacheHits != 1 || snapshot.ByUpstream["emby"] != 61 || snapshot.ByKind["openlist"] != 2 {
		t.Fatalf("restored breakdown = %+v", snapshot)
	}
	if len(snapshot.RecentEvents) != 50 {
		t.Fatalf("restored events = %d, want 50", len(snapshot.RecentEvents))
	}
}
