package ewf

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
)

var _ io.ReaderAt = (*Reader)(nil)

// ReadAt implements io.ReaderAt over the media. A read that reaches a chunk
// that cannot be decoded returns the bytes already copied from the chunks
// before it and an error wrapping ErrChunkCorrupt (a *ChunkError); the rest of
// p is left untouched, never zero-filled. A read past Size returns the bytes
// available and io.EOF. After Close it returns an error wrapping
// fs.ErrClosed. It is safe for concurrent use.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	if r.isClosed() {
		return 0, fmt.Errorf("ewf: read after Close: %w", fs.ErrClosed)
	}
	if off < 0 {
		return 0, errors.New("ewf: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	size := r.geo.size
	if off >= size {
		return 0, io.EOF
	}
	want, eof := len(p), false
	if avail := size - off; int64(want) > avail {
		want, eof = int(avail), true
	}
	cs := r.geo.chunkSize
	n := 0
	for n < want {
		pos := off + int64(n) // pos < size: no overflow
		data, err := r.chunk(pos / cs)
		if err != nil {
			return n, err
		}
		n += copy(p[n:want], data[pos%cs:])
	}
	if eof {
		return n, io.EOF
	}
	return n, nil
}
