package ewf

import (
	"context"
	"crypto/md5"  //nolint:gosec // MD5 is a value the format stores, compared for verification; not a security control
	"crypto/sha1" //nolint:gosec // SHA-1 is a value the format stores, compared for verification; not a security control
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
)

// HashStatus is the outcome of comparing one stored hash with the media.
type HashStatus string

const (
	// HashMatch: the computed hash equals the stored one.
	HashMatch HashStatus = "match"
	// HashMismatch: the media was read to the end and hashes differently.
	HashMismatch HashStatus = "mismatch"
	// HashAbsent: the container stores no such hash.
	HashAbsent HashStatus = "absent"
	// HashUnverified: the hash is stored, but the media could not be read to
	// the end, so nothing was compared.
	HashUnverified HashStatus = "unverified"
)

// HashCheck is one stored hash against the computed one (lower-case hex).
// Computed is empty unless the whole media was hashed (it is reported even
// when nothing is stored to compare it with).
type HashCheck struct {
	Stored, Computed string
	Status           HashStatus
}

// VerifyResult is the outcome of Verify.
type VerifyResult struct {
	Size        int64 // media size
	BytesHashed int64 // media bytes fed to the hashes
	MD5, SHA1   HashCheck
	// BadChunk is the index of the first chunk that could not be read, -1 when
	// there is none; BadChunkError is its error text.
	BadChunk      int64
	BadChunkError string
}

// Result summarises the verification as "mismatch" (either stored hash
// differs from the media), "unverified" (the media was not hashed to the
// end: a bad chunk or a cancelled run, whatever hashes are stored), "absent"
// (the container stores no hash) or "match". A mismatch outranks everything: it
// was seen on fully hashed media.
func (v VerifyResult) Result() string {
	switch {
	case v.MD5.Status == HashMismatch || v.SHA1.Status == HashMismatch:
		return string(HashMismatch)
	case v.BadChunk >= 0 || v.BytesHashed != v.Size || v.MD5.Status == HashUnverified || v.SHA1.Status == HashUnverified:
		return string(HashUnverified)
	case v.MD5.Status == HashAbsent && v.SHA1.Status == HashAbsent:
		return string(HashAbsent)
	}
	return string(HashMatch)
}

// Verify decodes the whole media once, chunk by chunk, and compares its MD5
// and SHA-1 with the hashes the container stores. Chunks are decoded straight
// from the segments, not through the read cache, so a verify does not evict
// the working set. ctx is checked before every chunk and progress (when not
// nil) is called after each with the bytes done and the media size.
//
// The first chunk that cannot be read stops the verification: the result
// names it, each stored hash is reported "unverified" with no computed value
// (nothing is ever compared on partial data) and the error is nil. An I/O error of
// the segment source is not a bad chunk: it is returned as the error with the
// partial result. A cancelled
// context returns the partial result (hashes "unverified") and ctx.Err().
func (r *Reader) Verify(ctx context.Context, progress func(done, total int64)) (VerifyResult, error) {
	res := VerifyResult{Size: r.geo.size, BadChunk: -1}
	res.MD5 = HashCheck{Stored: r.md5, Status: statusFor(r.md5)}
	res.SHA1 = HashCheck{Stored: r.sha1, Status: statusFor(r.sha1)}
	if r.isClosed() {
		return res, fmt.Errorf("ewf: verify after Close: %w", fs.ErrClosed)
	}
	m, s := md5.New(), sha1.New() //nolint:gosec // see the imports
	for idx := int64(0); idx < int64(r.geo.chunks); idx++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		data, err := r.decode(idx)
		if err != nil {
			if !errors.Is(err, ErrChunkCorrupt) {
				return res, err // an I/O failure of the segments: the run failed, no chunk is to blame
			}
			res.BadChunk, res.BadChunkError = idx, err.Error()
			return res, nil
		}
		_, _ = m.Write(data)
		_, _ = s.Write(data)
		res.BytesHashed += int64(len(data))
		if progress != nil {
			progress(res.BytesHashed, res.Size)
		}
	}
	if res.BytesHashed != res.Size {
		// Cannot happen for a validated geometry; never compare partial data.
		res.BadChunk, res.BadChunkError = int64(r.geo.chunks), fmt.Sprintf("chunks hold %d bytes, the media is %d", res.BytesHashed, res.Size)
		return res, nil
	}
	res.MD5.compare(hex.EncodeToString(m.Sum(nil)))
	res.SHA1.compare(hex.EncodeToString(s.Sum(nil)))
	return res, nil
}

// statusFor is the status before any comparison: unverified when a hash is
// stored, absent otherwise.
func statusFor(stored string) HashStatus {
	if stored == "" {
		return HashAbsent
	}
	return HashUnverified
}

// compare records the computed hash of fully hashed media. With no stored hash
// the computed value is still reported (the status stays absent).
func (c *HashCheck) compare(computed string) {
	c.Computed = computed
	switch {
	case c.Status == HashAbsent:
	case computed == c.Stored:
		c.Status = HashMatch
	default:
		c.Status = HashMismatch
	}
}
