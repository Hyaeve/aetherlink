// Package stats keeps in-memory counters and a rolling event log describing how
// media requests were served, so the admin UI can show 302 vs proxy behaviour
// without an external database.
package stats

import (
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
	mu         sync.RWMutex
	startedAt  time.Time
	counts     map[Outcome]uint64
	byKind     map[string]uint64
	byUpstream map[string]uint64
	cacheHits  uint64
	cacheMiss  uint64
	total      uint64
	events     []Event
	maxEvents  int
}

// New returns a collector that retains maxEvents recent events.
func New(maxEvents int) *Collector {
	if maxEvents < 20 {
		maxEvents = 20
	}
	return &Collector{
		startedAt:  time.Now(),
		counts:     map[Outcome]uint64{},
		byKind:     map[string]uint64{},
		byUpstream: map[string]uint64{},
		maxEvents:  maxEvents,
	}
}

// Record stores an event and updates counters.
func (c *Collector) Record(event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

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

	c.events = append(c.events, event)
	if len(c.events) > c.maxEvents {
		c.events = append([]Event(nil), c.events[len(c.events)-c.maxEvents:]...)
	}
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
