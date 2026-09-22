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
	restored  bool
}

// persistSettle 是「安静多久之后才把缓存写回磁盘」。播放器一次起播常连着发好几个
// 请求（探测、range、重试），每来一个就重写一遍整个文件没有意义；等安静下来再写
// 一次即可。它同时保证 put 不会等写盘 —— 写盘从一开始就不是同步做的。
const persistSettle = 400 * time.Millisecond

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

	// 写盘不挡在响应前面：put/purge 只改内存并往 dirty 里丢一个信号，由后台的
	// persistLoop 合并写入。dirty 是带缓冲的非阻塞通道，塞不进去就丢掉一次通知 ——
	// 丢的不是数据，写盘时读的是当下整张表。
	dirty     chan struct{}
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
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
	// 没有落盘路径（纯内存缓存）就不需要写盘协程：dirty/stop 保持 nil，
	// markDirty 对 nil 通道走 default，close 也直接返回。
	if persistencePath != "" {
		cache.dirty = make(chan struct{}, 1)
		cache.stop = make(chan struct{})
		cache.done = make(chan struct{})
		go cache.persistLoop()
	}
	return cache
}

func (c *lruCache) get(key string) (*Resolution, time.Duration, bool) {
	value, remaining, _, ok := c.getWithSource(key)
	return value, remaining, ok
}

func (c *lruCache) getWithSource(key string) (*Resolution, time.Duration, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return nil, 0, false, false
	}
	entry := element.Value.(*cacheEntry)
	remaining := time.Until(entry.expiresAt)
	if remaining <= 0 {
		c.order.Remove(element)
		delete(c.entries, key)
		return nil, 0, false, false
	}
	c.order.MoveToFront(element)
	restored := entry.restored
	entry.restored = false
	return entry.value, remaining, restored, true
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
	c.markDirty()
}

func (c *lruCache) purge() int {
	c.mu.Lock()
	count := c.order.Len()
	c.entries = make(map[string]*list.Element, c.maxSize)
	c.order.Init()
	c.mu.Unlock()
	c.markDirty()
	return count
}

func (c *lruCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// markDirty 通知后台把缓存写回磁盘。**非阻塞**，这是 put 在响应路径上唯一多做的
// 事 —— 客户端不必等一次「序列化整张表 + 写临时文件 + rename」。
func (c *lruCache) markDirty() {
	if c.dirty == nil {
		return
	}
	select {
	case c.dirty <- struct{}{}:
	default:
		// 已经有一次待写的通知了。丢掉的只是通知不是数据：写盘时读的是当下
		// 整张表，所以合并永远不会漏条目。
	}
}

// persistLoop 是唯一的写盘协程：收到通知后先等安静下来，写一次，再回去等下一次。
//
// 「安静结束」这一支也必须写 —— 它才是常态（客户端不再发请求了）。只有停止时才写，
// 等于正常跑一整天磁盘上一条新直链都不会有：真出现这种情况时不会有任何报错，
// 只是重启容器之后整张缓存凭空回到旧样子（TestPersistentCacheFlushesAfterQuietPeriod）。
func (c *lruCache) persistLoop() {
	defer close(c.done)
	for {
		select {
		case <-c.stop:
			c.persist()
			return
		case <-c.dirty:
			stopping := c.settle()
			c.persist()
			if stopping {
				return
			}
		}
	}
}

// settle 等到「安静 persistSettle 没人动缓存」再返回；期间每次新改动都重新计时。
// 返回 true 表示等待期间收到了停止信号，调用方应当补写一次再退出。
func (c *lruCache) settle() bool {
	timer := time.NewTimer(persistSettle)
	defer timer.Stop()
	for {
		select {
		case <-c.stop:
			return true
		case <-c.dirty:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(persistSettle)
		case <-timer.C:
			return false
		}
	}
}

// close 停掉写盘协程，并把还没落盘的改动补写一次。重建 resolver（保存配置）与
// 进程退出都会走这里，所以正常关闭不会丢缓存。
func (c *lruCache) close() {
	if c.stop == nil {
		return
	}
	c.closeOnce.Do(func() { close(c.stop) })
	<-c.done
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
		if os.IsNotExist(err) {
			logx.Infof("[resolver] 直链缓存文件不存在，首次播放将创建：%s", c.persistencePath)
		} else {
			logx.Warnf("[resolver] 读取直链缓存失败 %s：%v", c.persistencePath, err)
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
	skipped := len(entries)
	for _, persisted := range entries {
		if persisted.Key == "" || persisted.Value == nil || !persisted.ExpiresAt.After(now) {
			continue
		}
		element := c.order.PushBack(&cacheEntry{
			key:       persisted.Key,
			value:     persisted.Value,
			expiresAt: persisted.ExpiresAt,
			restored:  true,
		})
		c.entries[persisted.Key] = element
		restored++
		skipped--
		if c.order.Len() >= c.maxSize {
			break
		}
	}
	logx.Infof("[resolver] 读取直链缓存文件 %s：共 %d 条，恢复 %d 条，跳过 %d 条过期或无效记录", c.persistencePath, len(entries), restored, skipped)
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
