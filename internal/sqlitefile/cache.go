package sqlitefile

import (
	"container/list"
	"fmt"
	"sync"
	"sync/atomic"
)

// Stats are the work counters of a view.
type Stats struct {
	PageReads    int64 // page images fetched from the underlying source
	CacheHits    int64
	CellsParsed  int64
	PagesSkipped int64
}

// counters are the live, race-free counters behind Stats.
type counters struct {
	pageReads, cacheHits, cellsParsed, pagesSkipped atomic.Int64
}

func (c *counters) snapshot() Stats {
	return Stats{
		PageReads:    c.pageReads.Load(),
		CacheHits:    c.cacheHits.Load(),
		CellsParsed:  c.cellsParsed.Load(),
		PagesSkipped: c.pagesSkipped.Load(),
	}
}

type cacheEntry struct {
	pgno uint32
	data []byte
	loc  PageLoc
}

// pageCache is an LRU cache of page images over a pageSource. Its bytes
// (Limits.PageCacheBytes at most) are charged to the budget while a page is
// held and given back on eviction. Pages it returns are shared and read-only.
// It is safe for concurrent use.
type pageCache struct {
	e        *env
	src      pageSource
	pageSize int64
	maxBytes int64
	st       *counters

	mu   sync.Mutex
	used int64
	lru  *list.List // front = most recently used; values are *cacheEntry
	idx  map[uint32]*list.Element
}

func newPageCache(e *env, src pageSource, pageSize int, st *counters) *pageCache {
	if st == nil {
		st = &counters{}
	}
	return &pageCache{
		e: e, src: src, pageSize: int64(pageSize), maxBytes: e.opts.Limits.PageCacheBytes,
		st: st, lru: list.New(), idx: map[uint32]*list.Element{},
	}
}

func (c *pageCache) has(pgno uint32) bool { return c.src.has(pgno) }

// read serves pgno from the cache, or reads it from the source and keeps it.
func (c *pageCache) read(pgno uint32) (data []byte, loc PageLoc, err error) {
	c.mu.Lock()
	if el, ok := c.idx[pgno]; ok {
		c.lru.MoveToFront(el)
		ent := el.Value.(*cacheEntry)
		c.mu.Unlock()
		c.st.cacheHits.Add(1)
		return ent.data, ent.loc, nil
	}
	c.mu.Unlock()

	l := c.e.newLedger()
	defer l.guard(&err)
	if err := l.alloc(c.pageSize); err != nil {
		return nil, PageLoc{}, fmt.Errorf("page %d: %w", pgno, err)
	}
	c.e.at("cache.read")
	data, loc, err = c.src.read(pgno)
	if err != nil {
		return nil, PageLoc{}, err
	}
	c.st.pageReads.Add(1)
	if c.pageSize > c.maxBytes { // too big to keep: the charge was only for the read
		l.free(l.n)
		return data, loc, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[pgno]; ok { // another reader got there first: keep its copy
		l.free(l.n)
		c.lru.MoveToFront(el)
		ent := el.Value.(*cacheEntry)
		return ent.data, ent.loc, nil
	}
	for c.used+c.pageSize > c.maxBytes && c.lru.Len() > 0 {
		c.evictOldest()
	}
	c.idx[pgno] = c.lru.PushFront(&cacheEntry{pgno: pgno, data: data, loc: loc})
	c.used += c.pageSize
	return data, loc, nil // the charge now belongs to the cache entry
}

// evictOldest drops the least recently used page and gives its charge back.
// The caller holds c.mu.
func (c *pageCache) evictOldest() {
	el := c.lru.Back()
	ent := el.Value.(*cacheEntry)
	c.lru.Remove(el)
	delete(c.idx, ent.pgno)
	c.used -= c.pageSize
	c.e.budget.Free(c.pageSize)
}

// clear drops every cached page and gives the charges back.
func (c *pageCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.lru.Len() > 0 {
		c.evictOldest()
	}
}
