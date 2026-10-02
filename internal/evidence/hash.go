// Package evidence owns everything written into a case: hashing, the
// hash-chained audit log, the manifest, artifacts.db and verification.
package evidence

import (
	"crypto/md5" //nolint:gosec // MD5 is recorded alongside SHA-256 for interoperability with other forensic tools
	"crypto/sha256"
	"encoding/hex"
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
