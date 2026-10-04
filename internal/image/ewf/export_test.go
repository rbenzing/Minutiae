package ewf

// Caps exposed to the external test package.
const (
	MaxSegments    = maxSegments
	MaxHeaderText  = maxHeaderText
	MaxErrorRanges = maxErrorRanges
	MaxMetaValue   = maxMetaValue
)

// ErrorRanges returns the number of acquisition error ranges present in the
// image and the ranges kept.
func (r *Reader) ErrorRanges() (count uint64, kept [][2]uint32) {
	if r.err2 == nil {
		return 0, nil
	}
	return r.err2.count, r.err2.ranges
}

// SectionKinds lists, per segment, the section types in file order.
func (r *Reader) SectionKinds() [][]string {
	out := make([][]string, len(r.secs))
	for i, ss := range r.secs {
		for _, s := range ss {
			out[i] = append(out[i], s.label())
		}
	}
	return out
}

// Closed reports whether Close has been called.
func (r *Reader) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// SectionInfo is one section as the walk recorded it.
type SectionInfo struct {
	Type               string
	Offset, Next, Size int64
}

// Sections lists, per segment, every section in file order.
func (r *Reader) Sections() [][]SectionInfo {
	out := make([][]SectionInfo, len(r.secs))
	for i, ss := range r.secs {
		for _, s := range ss {
			out[i] = append(out[i], SectionInfo{Type: s.label(), Offset: s.off, Next: s.next, Size: s.size})
		}
	}
	return out
}

// CacheLen returns the number of chunks in the cache.
func (r *Reader) CacheLen() int { return r.cache.len() }

// CacheHits returns how many chunk reads the cache served.
func (r *Reader) CacheHits() uint64 { return r.cache.hitCount() }

// SetCacheCapacity changes the cache capacity (evicting down to it).
func (r *Reader) SetCacheCapacity(n int) { r.cache.setCapacity(n) }

// CacheCapacity returns the cache capacity.
func (r *Reader) CacheCapacity() int {
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	return r.cache.cap
}

// CacheCapacityFor returns the capacity chosen for a chunk size.
func CacheCapacityFor(chunkSize int64) int { return cacheCapacity(chunkSize) }

// ChunkKinds returns one letter per chunk: 'c' for a chunk the table marks
// compressed, 'u' for an uncompressed one.
func (r *Reader) ChunkKinds() string {
	b := make([]byte, len(r.refs))
	for i, ref := range r.refs {
		b[i] = 'u'
		if ref.compressed() {
			b[i] = 'c'
		}
	}
	return string(b)
}
