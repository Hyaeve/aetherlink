package resolver

import (
	"container/list"
	"sync"
	"time"
)

// cacheEntry is a resolved target plus its expiry.
type cacheEntry struct {
	key       string
	value     *Resolution
	expiresAt time.Time
}

// lruCache is a TTL + size bounded cache. Resolutions are cheap to recompute
// but each miss costs one upstream API round trip, so caching keeps seek-heavy
// players (which re-request the same track constantly) off the upstream API.
type lruCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxSize int
	entries map[string]*list.Element
	order   *list.List
}

func newLRUCache(ttl time.Duration, maxSize int) *lruCache {
	if maxSize <= 0 {
		maxSize = 1024
	}
	return &lruCache{
		ttl:     ttl,
		maxSize: maxSize,
		entries: make(map[string]*list.Element, maxSize),
		order:   list.New(),
	}
}

func (c *lruCache) get(key string) (*Resolution, time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return nil, 0, false
	}
	entry := element.Value.(*cacheEntry)
	remaining := time.Until(entry.expiresAt)
	if remaining <= 0 {
		c.order.Remove(element)
		delete(c.entries, key)
		return nil, 0, false
	}
	c.order.MoveToFront(element)
	return entry.value, remaining, true
}

func (c *lruCache) put(key string, value *Resolution, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		entry := element.Value.(*cacheEntry)
		entry.value = value
		entry.expiresAt = time.Now().Add(ttl)
		c.order.MoveToFront(element)
		return
	}
	element := c.order.PushFront(&cacheEntry{key: key, value: value, expiresAt: time.Now().Add(ttl)})
	c.entries[key] = element
	for c.order.Len() > c.maxSize {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*cacheEntry).key)
	}
}

func (c *lruCache) purge() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := c.order.Len()
	c.entries = make(map[string]*list.Element, c.maxSize)
	c.order.Init()
	return count
}

func (c *lruCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
