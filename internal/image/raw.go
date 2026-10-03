package image

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

const rawSectorSize = 512

// rawImage is a raw image made of one or more segment files that are
// logically concatenated.
type rawImage struct {
	files  []*os.File
	starts []int64 // starts[i] = cumulative byte offset of segment i
	sizes  []int64
	size   int64
	meta   []KV
}

func openRaw(files []*os.File) (*rawImage, error) {
	r := &rawImage{
		files:  files,
		starts: make([]int64, len(files)),
		sizes:  make([]int64, len(files)),
		meta:   []KV{{"segments", strconv.Itoa(len(files))}},
	}
	for i, f := range files {
		st, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("image: stat segment %d: %w", i+1, err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("image: segment %d (%s) is not a regular file", i+1, filepath.Base(f.Name()))
		}
		sz := st.Size()
		if len(files) > 1 && sz == 0 {
			return nil, fmt.Errorf("image: empty segment %d (%s)", i+1, filepath.Base(f.Name()))
		}
		if sz > math.MaxInt64-r.size {
			return nil, fmt.Errorf("image: total size overflows at segment %d", i+1)
		}
		r.starts[i] = r.size
		r.sizes[i] = sz
		r.size += sz
		r.meta = append(r.meta, KV{
			Key:   "segment." + strconv.Itoa(i+1),
			Value: fmt.Sprintf("%s (%d bytes)", filepath.Base(f.Name()), sz),
		})
	}
	return r, nil
}

func (r *rawImage) Size() int64     { return r.size }
func (r *rawImage) SectorSize() int { return rawSectorSize }
func (r *rawImage) Metadata() []KV  { return append([]KV(nil), r.meta...) }

func (r *rawImage) Format() string {
	if len(r.files) > 1 {
		return "split-raw"
	}
	return "raw"
}

// Close closes every segment file and returns the first error.
func (r *rawImage) Close() error {
	var first error
	for _, f := range r.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ReadAt implements io.ReaderAt: a read that reaches beyond Size returns the
// bytes available and io.EOF.
func (r *rawImage) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("image: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	// Overflow-safe: off < size, so size-off is positive.
	want := len(p)
	short := false
	if int64(want) > r.size-off {
		want = int(r.size - off)
		short = true
	}

	// Starting segment: the last one whose start is <= off.
	i := sort.Search(len(r.starts), func(i int) bool { return r.starts[i] > off }) - 1
	total := 0
	for total < want {
		segOff := off + int64(total) - r.starts[i]
		chunk := want - total
		if left := r.sizes[i] - segOff; int64(chunk) > left {
			chunk = int(left)
		}
		n, err := r.files[i].ReadAt(p[total:total+chunk], segOff)
		total += n
		if n < chunk {
			if err == nil || errors.Is(err, io.EOF) {
				// the segment is shorter than when it was opened
				err = io.ErrUnexpectedEOF
			}
			return total, err
		}
		i++
	}
	if short {
		return total, io.EOF
	}
	return total, nil
}
