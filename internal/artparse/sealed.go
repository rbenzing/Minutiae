package artparse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	data []byte            // non-nil when held in memory
	f    artFile           // else the read-only handle
	sr   *io.SectionReader // the only view of f a reader gets: never past the verified size
}

func (s *source) ReadAt(p []byte, off int64) (int, error) {
	if s.sr != nil {
		return s.sr.ReadAt(p, off)
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

func (s *sealSet) wrap(r io.ReaderAt, budget *parse.ReadBudget) *parse.SealedReaderAt {
	w := parse.NewSealedReaderAtShared(r, budget)
	s.add(w)
	return w
}

// add registers a reader made elsewhere (sealed at once when the set already is).
func (s *sealSet) add(w *parse.SealedReaderAt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		w.Seal()
	}
	s.rs = append(s.rs, w)
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

// classifyOpenError turns an open failure of an artifact the run's snapshot holds into an integrity
// failure when the manifest itself is the cause: its record is gone or the manifest can no longer be
// read. The manifest changed under the run; that is never a plain I/O failure.
func (h *Host) classifyOpenError(snap *Snapshot, id string, err error) error {
	if errors.Is(err, evidence.ErrIntegrity) {
		return err
	}
	if _, inSnap := snap.Record(id); !inSnap {
		return err
	}
	if errors.Is(err, evidence.ErrUnknownArtifact) {
		return integrityf("artifact %s: its manifest record is gone (%v)", id, err)
	}
	if _, merr := h.c.Manifest(); merr != nil {
		return integrityf("artifact %s: the manifest can no longer be read (%v)", id, merr)
	}
	return err
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
	of, rec, err := h.c.OpenArtifact(id)
	if err != nil {
		return nil, h.classifyOpenError(snap, id, err)
	}
	var f artFile = of
	if h.wrapFile != nil {
		f = h.wrapFile(f)
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
		n, rerr := f.Read(extra[:])
		if n > 0 {
			return nil, integrityf("artifact %s (%s): the file grew after its size was checked", id, rec.Path)
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return nil, rerr // an I/O failure says nothing about the evidence
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
		s.f, s.sr = f, io.NewSectionReader(f, 0, size)
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

// hashNow hashes the artifact on disk now, through a fresh OpenArtifact (so a replaced file is seen,
// not only an edited one), and returns the hash with the case path it was read from.
func (h *Host) hashNow(ctx context.Context, rec evidence.ManifestRecord) (hash, path string, err error) {
	f, now, err := h.c.OpenArtifact(rec.ID)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, ctxReader{ctx, f}); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), now.Path, nil
}

// artFile is what the host needs of an opened artifact file.
type artFile interface {
	io.Reader
	io.ReaderAt
	io.Closer
}
