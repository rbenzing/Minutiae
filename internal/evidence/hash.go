// Package evidence owns everything written into a case: hashing, the
// hash-chained audit log, the manifest, artifacts.db and verification.
package evidence

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5 is recorded alongside SHA-256 for interoperability with other forensic tools
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
)

// Digests are the hashes and byte count of a stream.
type Digests struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	MD5    string `json:"md5"`
}

// MultiHasher computes SHA-256 and MD5 over everything written to it.
type MultiHasher struct {
	sha  hash.Hash
	md5  hash.Hash
	size int64
}

// NewMultiHasher returns an empty hasher.
func NewMultiHasher() *MultiHasher {
	return &MultiHasher{sha: sha256.New(), md5: md5.New()} //nolint:gosec // see import comment
}

// Write never fails; hash.Hash writes cannot return errors.
func (m *MultiHasher) Write(p []byte) (int, error) {
	_, _ = m.sha.Write(p)
	_, _ = m.md5.Write(p)
	m.size += int64(len(p))
	return len(p), nil
}

// Sum returns the digests of everything written so far.
func (m *MultiHasher) Sum() Digests {
	return Digests{
		Size:   m.size,
		SHA256: hex.EncodeToString(m.sha.Sum(nil)),
		MD5:    hex.EncodeToString(m.md5.Sum(nil)),
	}
}

// HashFile streams the file at path through a MultiHasher.
func HashFile(path string) (Digests, error) {
	f, err := os.Open(path)
	if err != nil {
		return Digests{}, err
	}
	defer func() { _ = f.Close() }()
	h := NewMultiHasher()
	if _, err := io.Copy(h, f); err != nil {
		return Digests{}, err
	}
	return h.Sum(), nil
}

// ctxReader fails a Read once its context has ended, so a long copy stops within one read of a cancel.
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

// hashReaderContext streams r through a MultiHasher and stops with ctx.Err() soon after ctx ends.
func hashReaderContext(ctx context.Context, r io.Reader) (Digests, error) {
	h := NewMultiHasher()
	if _, err := io.Copy(h, ctxReader{ctx: ctx, r: r}); err != nil {
		return Digests{}, err
	}
	return h.Sum(), nil
}

// hashFileContext is HashFile that a cancelled context interrupts (case verify re-hashes every artifact,
// which can be many gigabytes).
func hashFileContext(ctx context.Context, path string) (Digests, error) {
	f, err := os.Open(path)
	if err != nil {
		return Digests{}, err
	}
	defer func() { _ = f.Close() }()
	return hashReaderContext(ctx, f)
}

// ctxCaused reports whether err is the end of the verify context ctx: ctx must really have ended and err
// must be a context error, so a Canceled wrapped by some other reader is still a problem.
func ctxCaused(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}
