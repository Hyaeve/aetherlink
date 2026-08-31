package resolver

import (
	"container/list"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aetherlink/aetherlink/internal/logx"
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
	mu              sync.Mutex
	persistMu       sync.Mutex
	ttl             time.Duration
	maxSize         int
	persistencePath string
	entries         map[string]*list.Element
	order           *list.List
}

func newLRUCache(ttl time.Duration, maxSize int) *lruCache {
	return newPersistentLRUCache(ttl, maxSize, "")
}

func newPersistentLRUCache(ttl time.Duration, maxSize int, persistencePath string) *lruCache {
	if maxSize <= 0 {
		maxSize = 1024
	}
	cache := &lruCache{
		ttl:             ttl,
		maxSize:         maxSize,
		persistencePath: persistencePath,
		entries:         make(map[string]*list.Element, maxSize),
		order:           list.New(),
	}
	cache.load()
	return cache
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
	if element, ok := c.entries[key]; ok {
		entry := element.Value.(*cacheEntry)
		entry.value = value
		entry.expiresAt = time.Now().Add(ttl)
		c.order.MoveToFront(element)
	} else {
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
	c.mu.Unlock()
	c.persist()
}

func (c *lruCache) purge() int {
	c.mu.Lock()
	count := c.order.Len()
	c.entries = make(map[string]*list.Element, c.maxSize)
	c.order.Init()
	c.mu.Unlock()
	c.persist()
	return count
}

func (c *lruCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

type persistedCacheEntry struct {
	Key       string      `json:"key"`
	Value     *Resolution `json:"value"`
	ExpiresAt time.Time   `json:"expiresAt"`
}

func (c *lruCache) load() {
	if c.persistencePath == "" {
		return
	}
	data, err := os.ReadFile(c.persistencePath)
	if err != nil {
		if !os.IsNotExist(err) {
			logx.Warnf("[resolver] 读取直链缓存失败: %v", err)
		}
		return
	}
	var entries []persistedCacheEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		logx.Warnf("[resolver] 解析直链缓存失败: %v", err)
		return
	}
	now := time.Now()
	restored := 0
	for _, persisted := range entries {
		if persisted.Key == "" || persisted.Value == nil || !persisted.ExpiresAt.After(now) {
			continue
		}
		element := c.order.PushBack(&cacheEntry{
			key:       persisted.Key,
			value:     persisted.Value,
			expiresAt: persisted.ExpiresAt,
		})
		c.entries[persisted.Key] = element
		restored++
		if c.order.Len() >= c.maxSize {
			break
		}
	}
	if restored > 0 {
		logx.Infof("[resolver] 已恢复 %d 条未过期直链缓存（%s）", restored, c.persistencePath)
	}
}

func (c *lruCache) persist() {
	if c.persistencePath == "" {
		return
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()

	c.mu.Lock()
	entries := make([]persistedCacheEntry, 0, c.order.Len())
	for element := c.order.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*cacheEntry)
		if entry.expiresAt.After(time.Now()) {
			entries = append(entries, persistedCacheEntry{
				Key:       entry.key,
				Value:     entry.value,
				ExpiresAt: entry.expiresAt,
			})
		}
	}
	c.mu.Unlock()

	directory := filepath.Dir(c.persistencePath)
	temporary, err := os.CreateTemp(directory, ".aetherlink-cache-*.tmp")
	if err != nil {
		logx.Warnf("[resolver] 创建直链缓存临时文件失败: %v", err)
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err == nil {
		encoder := json.NewEncoder(temporary)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(entries)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		logx.Warnf("[resolver] 写入直链缓存失败: %v", err)
		return
	}
	if err := os.Rename(temporaryPath, c.persistencePath); err != nil {
		logx.Warnf("[resolver] 保存直链缓存失败: %v", err)
	}
}
