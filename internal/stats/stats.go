// Package stats keeps counters and a rolling event log describing how media
// requests were served. The optional persistence file lets the admin UI retain
// meaningful playback statistics across process restarts.
package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Outcome describes how a single media request ended.
type Outcome string

const (
	OutcomeRedirect     Outcome = "redirect"
	OutcomeProxyStream  Outcome = "proxy"
	OutcomeLocalFile    Outcome = "local"
	OutcomePassthrough  Outcome = "passthrough"
	OutcomeTranscode    Outcome = "transcode"
	OutcomeError        Outcome = "error"
	OutcomeUnauthorized Outcome = "unauthorized"
)

// Event is one recorded media request.
type Event struct {
	Time               time.Time     `json:"time"`
	Upstream           string        `json:"upstream"`
	Path               string        `json:"path"`
	ItemID             string        `json:"itemId,omitempty"`
	FileID             string        `json:"fileId,omitempty"`
	MediaPath          string        `json:"mediaPath,omitempty"`
	Target             string        `json:"target,omitempty"`
	Kind               string        `json:"kind,omitempty"`
	Outcome            Outcome       `json:"outcome"`
	StatusCode         int           `json:"statusCode"`
	Duration           time.Duration `json:"durationMs"`
	CacheHit           bool          `json:"cacheHit"`
	CacheSource        string        `json:"cacheSource,omitempty"`
	CacheTTLSeconds    int64         `json:"cacheTtlSeconds,omitempty"`
	Client             string        `json:"client,omitempty"`
	UserAgent          string        `json:"userAgent,omitempty"`
	EffectiveUserAgent string        `json:"effectiveUserAgent,omitempty"`
	Error              string        `json:"error,omitempty"`
}

// Snapshot is the aggregated view returned by the admin API.
type Snapshot struct {
	StartedAt     time.Time         `json:"startedAt"`
	UptimeSeconds int64             `json:"uptimeSeconds"`
	TotalRequests uint64            `json:"totalRequests"`
	Redirects     uint64            `json:"redirects"`
	ProxyStreams  uint64            `json:"proxyStreams"`
	Transcodes    uint64            `json:"transcodes"`
	LocalFiles    uint64            `json:"localFiles"`
	Passthroughs  uint64            `json:"passthroughs"`
	Errors        uint64            `json:"errors"`
	Unauthorized  uint64            `json:"unauthorized"`
	CacheHits     uint64            `json:"cacheHits"`
	CacheMisses   uint64            `json:"cacheMisses"`
	ByKind        map[string]uint64 `json:"byKind"`
	ByUpstream    map[string]uint64 `json:"byUpstream"`
	RecentEvents  []Event           `json:"recentEvents"`
}

// Collector is a concurrency-safe stats sink.
type Collector struct {
	mu              sync.RWMutex
	startedAt       time.Time
	counts          map[Outcome]uint64
	byKind          map[string]uint64
	byUpstream      map[string]uint64
	cacheHits       uint64
	cacheMiss       uint64
	total           uint64
	events          []Event
	maxEvents       int
	persistencePath string
}

type persistedSnapshot struct {
	Events     []Event            `json:"events"`
	Counts     map[Outcome]uint64 `json:"counts,omitempty"`
	ByKind     map[string]uint64  `json:"byKind,omitempty"`
	ByUpstream map[string]uint64  `json:"byUpstream,omitempty"`
	CacheHits  uint64             `json:"cacheHits,omitempty"`
	CacheMiss  uint64             `json:"cacheMisses,omitempty"`
	Total      uint64             `json:"total,omitempty"`
}

// New returns a collector that retains maxEvents recent events.
func New(maxEvents int) *Collector {
	return NewWithPersistence(maxEvents, "")
}

// NewWithPersistence restores recent playback events from disk and keeps the
// aggregate counters aligned with those events across process restarts.
func NewWithPersistence(maxEvents int, persistencePath string) *Collector {
	if maxEvents < 20 {
		maxEvents = 20
	}
	collector := &Collector{
		startedAt:       time.Now(),
		counts:          map[Outcome]uint64{},
		byKind:          map[string]uint64{},
		byUpstream:      map[string]uint64{},
		maxEvents:       maxEvents,
		persistencePath: persistencePath,
	}
	collector.load()
	return collector
}

// Record stores an event and updates counters.
func (c *Collector) Record(event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.recordCountsLocked(event)

	c.events = append(c.events, event)
	if len(c.events) > c.maxEvents {
		c.events = append([]Event(nil), c.events[len(c.events)-c.maxEvents:]...)
	}
	c.persistLocked()
}

func (c *Collector) recordCountsLocked(event Event) {
	c.total++
	c.counts[event.Outcome]++
	if event.Kind != "" {
		c.byKind[event.Kind]++
	}
	if event.Upstream != "" {
		c.byUpstream[event.Upstream]++
	}
	if event.Outcome != OutcomePassthrough {
		if event.CacheHit {
			c.cacheHits++
		} else {
			c.cacheMiss++
		}
	}
}

func (c *Collector) load() {
	if c.persistencePath == "" {
		return
	}
	data, err := os.ReadFile(c.persistencePath)
	if err != nil {
		return
	}
	var persisted persistedSnapshot
	if err := json.Unmarshal(data, &persisted); err != nil {
		return
	}
	if len(persisted.Events) > c.maxEvents {
		persisted.Events = persisted.Events[len(persisted.Events)-c.maxEvents:]
	}
	if persisted.Counts != nil || persisted.Total > 0 {
		for outcome, count := range persisted.Counts {
			c.counts[outcome] = count
		}
		c.byKind = persisted.ByKind
		if c.byKind == nil {
			c.byKind = map[string]uint64{}
		}
		c.byUpstream = persisted.ByUpstream
		if c.byUpstream == nil {
			c.byUpstream = map[string]uint64{}
		}
		c.cacheHits = persisted.CacheHits
		c.cacheMiss = persisted.CacheMiss
		c.total = persisted.Total
	} else {
		for _, event := range persisted.Events {
			c.recordCountsLocked(event)
		}
	}
	c.events = append(c.events, persisted.Events...)
}

func (c *Collector) persistLocked() {
	if c.persistencePath == "" {
		return
	}
	data, err := json.Marshal(persistedSnapshot{
		Events:     c.events,
		Counts:     c.counts,
		ByKind:     c.byKind,
		ByUpstream: c.byUpstream,
		CacheHits:  c.cacheHits,
		CacheMiss:  c.cacheMiss,
		Total:      c.total,
	})
	if err != nil {
		return
	}
	directory := filepath.Dir(c.persistencePath)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return
	}
	temporary, err := os.CreateTemp(directory, ".playback-stats-*.tmp")
	if err != nil {
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return
	}
	if err := temporary.Close(); err != nil {
		return
	}
	_ = os.Rename(temporaryPath, c.persistencePath)
}

// Snapshot returns the current aggregate view with the newest events first.
func (c *Collector) Snapshot(eventLimit int) Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if eventLimit <= 0 || eventLimit > len(c.events) {
		eventLimit = len(c.events)
	}
	recent := make([]Event, 0, eventLimit)
	for i := len(c.events) - 1; i >= len(c.events)-eventLimit; i-- {
		recent = append(recent, c.events[i])
	}

	snapshot := Snapshot{
		StartedAt:     c.startedAt,
		UptimeSeconds: int64(time.Since(c.startedAt).Seconds()),
		TotalRequests: c.total,
		Redirects:     c.counts[OutcomeRedirect],
		ProxyStreams:  c.counts[OutcomeProxyStream],
		Transcodes:    c.counts[OutcomeTranscode],
		LocalFiles:    c.counts[OutcomeLocalFile],
		Passthroughs:  c.counts[OutcomePassthrough],
		Errors:        c.counts[OutcomeError],
		Unauthorized:  c.counts[OutcomeUnauthorized],
		CacheHits:     c.cacheHits,
		CacheMisses:   c.cacheMiss,
		ByKind:        copyCounts(c.byKind),
		ByUpstream:    copyCounts(c.byUpstream),
		RecentEvents:  recent,
	}
	return snapshot
}

// TopKinds returns kinds ordered by count, used for compact UI summaries.
func (c *Collector) TopKinds() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	kinds := make([]string, 0, len(c.byKind))
	for kind := range c.byKind {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return c.byKind[kinds[i]] > c.byKind[kinds[j]] })
	return kinds
}

func copyCounts(source map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}
