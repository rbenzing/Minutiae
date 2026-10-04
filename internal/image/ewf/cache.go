package ewf

import (
	"container/list"
	"sync"
)

const (
	maxCacheEntries = 64
	cacheBudget     = 128 << 20 // bytes; the entry count shrinks for large chunks
)

// cacheCapacity is min(64, max(1, 128 MiB / chunkSize)) entries.
func cacheCapacity(chunkSize int64) int {
	if chunkSize <= 0 {
		return 1
	}
	return int(min(maxCacheEntries, max(1, cacheBudget/chunkSize)))
}

// chunkCache is a mutex-protected LRU of decoded chunks. Stored slices are
// immutable: nobody writes to them after insertion, so readers may copy from a
// slice they got before it was evicted.
type chunkCache struct {
	mu     sync.Mutex
	cap    int
	ll     *list.List // front = most recently used; values are *cacheEntry
	m      map[int64]*list.Element
	hits   uint64
	closed bool
}

type cacheEntry struct {
	idx  int64
	data []byte
}

func newChunkCache(capacity int) *chunkCache {
	return &chunkCache{cap: max(1, capacity), ll: list.New(), m: map[int64]*list.Element{}}
}

func (c *chunkCache) get(idx int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[idx]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.hits++
	return el.Value.(*cacheEntry).data, true
}

// put inserts data (the last insert for an idx wins) and evicts the least
// recently used entries beyond the capacity. After close it drops the data.
func (c *chunkCache) put(idx int64, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if el, ok := c.m[idx]; ok {
		el.Value.(*cacheEntry).data = data
		c.ll.MoveToFront(el)
		return
	}
	c.m[idx] = c.ll.PushFront(&cacheEntry{idx: idx, data: data})
	c.trim()
}

func (c *chunkCache) trim() {
	for c.ll.Len() > c.cap {
		el := c.ll.Back()
		c.ll.Remove(el)
		delete(c.m, el.Value.(*cacheEntry).idx)
	}
}

func (c *chunkCache) setCapacity(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cap = max(1, n)
	c.trim()
}

func (c *chunkCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *chunkCache) hitCount() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

// close empties the cache and makes later inserts no-ops.
func (c *chunkCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.ll.Init()
	clear(c.m)
}
