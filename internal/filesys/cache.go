package filesys

import (
	"container/list"
	"errors"
	"io"
	"sync"
)

type cachedBlock struct {
	idx  int64
	data []byte // valid bytes of the block; shorter than the block size at the end of the source
}

type cachedReader struct {
	src       io.ReaderAt
	blockSize int64
	capacity  int

	mu     sync.Mutex
	blocks map[int64]*list.Element
	lru    *list.List // front = most recently used
}

// NewCachedReader returns a reader that caches aligned blocks of blockSize
// bytes from r, evicting the least recently used when more than capacity
// blocks are held. It is safe for concurrent use. Reads return exactly what r
// returned: a block that r returned short is cached short, so no data past
// the end of the source is ever invented. A blockSize or capacity below 1 is
// treated as 512 or 1.
func NewCachedReader(r io.ReaderAt, blockSize int, capacity int) io.ReaderAt {
	if blockSize < 1 {
		blockSize = 512
	}
	if capacity < 1 {
		capacity = 1
	}
	return &cachedReader{
		src:       r,
		blockSize: int64(blockSize),
		capacity:  capacity,
		blocks:    map[int64]*list.Element{},
		lru:       list.New(),
	}
}

func (c *cachedReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("filesys: negative read offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for n < len(p) {
		pos, ok := AddOK(off, int64(n))
		if !ok {
			return n, io.EOF
		}
		idx := pos / c.blockSize
		blk, err := c.block(idx)
		if in := pos - idx*c.blockSize; in < int64(len(blk)) {
			n += copy(p[n:], blk[in:])
		}
		if err != nil {
			return n, err
		}
		if n < len(p) && int64(len(blk)) < c.blockSize {
			return n, io.EOF // short block: the source ended inside it
		}
	}
	return n, nil
}

// block returns the cached valid bytes of block idx, loading it on a miss.
// The caller holds c.mu. A non-nil error is returned with whatever bytes the
// source produced; such blocks are not cached.
func (c *cachedReader) block(idx int64) ([]byte, error) {
	if el, ok := c.blocks[idx]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*cachedBlock).data, nil
	}
	start, ok := MulOK(idx, c.blockSize)
	if !ok {
		return nil, io.EOF
	}
	buf := make([]byte, c.blockSize)
	if c.lru.Len() >= c.capacity {
		el := c.lru.Back()
		old := el.Value.(*cachedBlock)
		delete(c.blocks, old.idx)
		c.lru.Remove(el)
		buf = old.data[:cap(old.data)] // reuse the evicted buffer
	}
	n, err := c.src.ReadAt(buf, start)
	switch {
	case err == nil && n < len(buf):
		return buf[:n], io.ErrUnexpectedEOF // a ReaderAt must not short-read silently
	case err != nil && !errors.Is(err, io.EOF):
		return buf[:n], err
	}
	cb := &cachedBlock{idx: idx, data: buf[:n]} // io.EOF with a full or short block is a normal end of source
	c.blocks[idx] = c.lru.PushFront(cb)
	return cb.data, nil
}
