package ewf

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"sync"
)

// maxCompressedSlack is added to twice the chunk size to bound how many
// bytes of a compressed chunk are read (zlib can expand incompressible data
// slightly).
const maxCompressedSlack = 1024

// zlibPool recycles inflaters; a pooled reader is reset onto every new stream.
var zlibPool sync.Pool

// chunkLen is the media bytes in chunk idx: the chunk size, except for the
// last chunk of the media.
func (r *Reader) chunkLen(idx int64) int64 {
	return min(r.geo.chunkSize, r.geo.size-idx*r.geo.chunkSize)
}

// chunk returns the decoded bytes of chunk idx. The slice is shared with the
// cache and must not be modified. Failures are never cached.
func (r *Reader) chunk(idx int64) ([]byte, error) {
	if d, ok := r.cache.get(idx); ok {
		return d, nil
	}
	// Decoding happens outside the cache lock: two goroutines may decode the
	// same chunk, and the results are identical.
	d, err := r.decode(idx)
	if err != nil {
		return nil, err
	}
	r.cache.put(idx, d)
	return d, nil
}

func chunkErr(idx int64, ref chunkRef, cause error) error {
	e := &ChunkError{Chunk: idx, Segment: ref.seg() + 1, Offset: -1, Err: cause}
	if !ref.unlocated() {
		e.Offset = ref.off()
	}
	return e
}

// decode reads and verifies chunk idx from its segment. Every failure is a
// *ChunkError wrapping ErrChunkCorrupt.
func (r *Reader) decode(idx int64) ([]byte, error) {
	if idx < 0 || idx >= int64(r.geo.chunks) {
		return nil, &ChunkError{Chunk: idx, Offset: -1, Err: errors.New("chunk index outside the media")}
	}
	if idx >= int64(len(r.refs)) {
		if r.gap != nil {
			return nil, &ChunkError{Chunk: idx, Offset: -1, Err: errors.New(r.gap.why)}
		}
		return nil, &ChunkError{Chunk: idx, Offset: -1, Err: fmt.Errorf("no table entry (the chunk table covers %d of %d chunks)", len(r.refs), r.geo.chunks)}
	}
	ref := r.refs[idx]
	if ref.disputed() {
		return nil, chunkErr(idx, ref, errors.New("table and table2 disagree about this chunk and it passes its integrity check at neither location"))
	}
	if ref.outside() {
		return nil, chunkErr(idx, ref, errors.New("table entry points outside the segment"))
	}
	seg := r.segs[ref.seg()]
	off := ref.off()
	mediaLen := r.chunkLen(idx)

	if !ref.compressed() {
		buf := make([]byte, mediaLen+4)
		if err := readFull(seg.R, buf, off); err != nil {
			return nil, chunkErr(idx, ref, err)
		}
		if got, want := binary.LittleEndian.Uint32(buf[mediaLen:]), adler32.Checksum(buf[:mediaLen]); got != want {
			return nil, chunkErr(idx, ref, fmt.Errorf("checksum mismatch (stored %#08x, computed %#08x)", got, want))
		}
		return buf[:mediaLen:mediaLen], nil
	}

	n := min(r.chunkEnd(idx, ref, seg.Size)-off, 2*r.geo.chunkSize+maxCompressedSlack)
	if n <= 0 {
		return nil, chunkErr(idx, ref, errors.New("no bytes available for the compressed chunk"))
	}
	src := make([]byte, n)
	if err := readFull(seg.R, src, off); err != nil {
		return nil, chunkErr(idx, ref, err)
	}
	out, err := inflateExact(src, mediaLen)
	if err != nil {
		return nil, chunkErr(idx, ref, err)
	}
	return out, nil
}

// chunkEnd is where the compressed chunk idx at ref may end: the next
// entry's offset when that is in the same segment and larger, and never past
// the end of the sectors section holding it (or the segment, when none does).
func (r *Reader) chunkEnd(idx int64, ref chunkRef, segSize int64) int64 {
	off := ref.off()
	end := segSize
	if sp, ok := spanOf(r.sectors[ref.seg()], off); ok {
		end = min(end, sp.end)
	}
	if idx+1 < int64(len(r.refs)) {
		nx := r.refs[idx+1]
		if nx.seg() == ref.seg() && !nx.unlocated() && nx.off() > off {
			end = min(end, nx.off())
		}
	}
	return end
}

// inflateExact inflates the zlib stream in src to exactly want bytes. The
// output is capped at want+1 bytes however large the stream claims to be, and
// reading to EOF verifies the stream's own Adler-32 trailer.
func inflateExact(src []byte, want int64) ([]byte, error) {
	br := bytes.NewReader(src)
	var zr io.ReadCloser
	if v, ok := zlibPool.Get().(io.ReadCloser); ok {
		if err := v.(zlib.Resetter).Reset(br, nil); err != nil {
			return nil, fmt.Errorf("zlib header: %w", err)
		}
		zr = v
	} else {
		var err error
		if zr, err = zlib.NewReader(br); err != nil {
			return nil, fmt.Errorf("zlib header: %w", err)
		}
	}
	defer zlibPool.Put(zr)
	lr := io.LimitReader(zr, want+1)
	out := make([]byte, want)
	if _, err := io.ReadFull(lr, out); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("zlib stream ends before the chunk's %d bytes: %w", want, err)
		}
		return nil, fmt.Errorf("zlib stream: %w", err)
	}
	var probe [1]byte
	for range 8 {
		m, err := lr.Read(probe[:])
		if m > 0 {
			return nil, fmt.Errorf("zlib stream inflates to more than the chunk's %d bytes", want)
		}
		if err == io.EOF {
			return out[:want:want], nil
		}
		if err != nil {
			return nil, fmt.Errorf("zlib stream: %w", err)
		}
	}
	return nil, errors.New("zlib stream does not end")
}
