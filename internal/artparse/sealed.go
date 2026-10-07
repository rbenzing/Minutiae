package artparse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// HashMismatchError says an artifact's bytes no longer hash to the SHA-256 the
// manifest recorded. It is an integrity failure (Unwrap gives
// evidence.ErrIntegrity).
type HashMismatchError struct{ ArtifactID, Path, Want, Got string }

func (e *HashMismatchError) Error() string {
	return fmt.Sprintf("artifact %s (%s): sha256 is %s, the manifest records %s", e.ArtifactID, e.Path, e.Got, e.Want)
}

// Unwrap returns evidence.ErrIntegrity.
func (e *HashMismatchError) Unwrap() error { return evidence.ErrIntegrity }

// ctxReader stops a long read when the context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// source is one verified artifact the bundle serves: its bytes in memory, or a
// read-only handle that was pre-hashed. It is never handed to a parser; only
// the sealed readers wrapped around it are.
type source struct {
	rec  evidence.ManifestRecord
	data []byte   // non-nil when held in memory
	f    *os.File // else the read-only handle
}

func (s *source) ReadAt(p []byte, off int64) (int, error) {
	if s.f != nil {
		return s.f.ReadAt(p, off)
	}
	if off < 0 {
		return 0, fmt.Errorf("negative offset %d", off)
	}
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *source) close() {
	if s.f != nil {
		_ = s.f.Close()
	}
}

// sealSet holds every sealed reader handed out, so seal reaches them all and a
// reader made after the seal is sealed at once.
type sealSet struct {
	mu     sync.Mutex
	rs     []*parse.SealedReaderAt
	sealed bool
}

func (s *sealSet) wrap(r io.ReaderAt, probeLimit int64) *parse.SealedReaderAt {
	w := parse.NewSealedReaderAt(r, probeLimit)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		w.Seal()
	}
	s.rs = append(s.rs, w)
	return w
}

func (s *sealSet) seal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealed = true
	for _, r := range s.rs {
		r.Seal()
	}
}

func (s *sealSet) isSealed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealed
}

// integrityf wraps evidence.ErrIntegrity.
func integrityf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", evidence.ErrIntegrity, fmt.Sprintf(format, args...))
}

// loadSource opens artifact id through Case.OpenArtifact (regular file inside
// the case, size equal to the manifest), requires the record to equal the
// snapshot's, recomputes its SHA-256 and compares it with the manifest. An
// artifact that fits memLeft is read once, hashed while read and served from
// memory (no window between check and use); a larger one is pre-hashed in one
// sequential pass and served from the read-only handle. memLeft is the
// in-memory budget still free (a value above memMax is treated as memMax).
func (h *Host) loadSource(ctx context.Context, snap *Snapshot, id string, memMax, memLeft int64) (*source, error) {
	if h.onOpen != nil {
		h.onOpen(id)
	}
	f, rec, err := h.c.OpenArtifact(id)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
		}
	}()
	if h.afterSize != nil {
		h.afterSize(id)
	}
	if !snap.Matches(rec) {
		return nil, integrityf("artifact %s: the manifest changed during the run", id)
	}
	hash := sha256.New()
	size := rec.Size
	s := &source{rec: rec}
	if size <= min(memMax, memLeft) {
		buf := make([]byte, size)
		r := ctxReader{ctx, io.TeeReader(f, hash)}
		if _, err := io.ReadFull(r, buf); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return nil, integrityf("artifact %s (%s): the file is shorter than its manifest size %d", id, rec.Path, size)
			}
			return nil, err
		}
		var extra [1]byte
		if n, _ := f.Read(extra[:]); n > 0 {
			return nil, integrityf("artifact %s (%s): the file grew after its size was checked", id, rec.Path)
		}
		s.data = buf
	} else {
		n, err := io.Copy(hash, ctxReader{ctx, f})
		if err != nil {
			return nil, err
		}
		if n != size {
			return nil, integrityf("artifact %s (%s): read %d bytes, the manifest size is %d", id, rec.Path, n, size)
		}
		s.f = f
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != rec.SHA256 {
		return nil, &HashMismatchError{ArtifactID: id, Path: rec.Path, Want: rec.SHA256, Got: got}
	}
	if s.f == nil {
		_ = f.Close()
	}
	ok = true
	return s, nil
}

// rehash hashes the artifact on disk now, through a fresh OpenArtifact (so a
// replaced file is seen, not only an edited one).
func (h *Host) rehash(ctx context.Context, rec evidence.ManifestRecord) error {
	f, now, err := h.c.OpenArtifact(rec.ID)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, ctxReader{ctx, f}); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != rec.SHA256 {
		return &HashMismatchError{ArtifactID: rec.ID, Path: now.Path, Want: rec.SHA256, Got: got}
	}
	return nil
}
