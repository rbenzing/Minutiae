package examine

import (
	"context"
	"errors"

	"github.com/rbenzing/minutiae/internal/image"
)

// ErrNoStoredHashes is returned by VerifyContainer for a container that stores
// no hashes (raw images).
var ErrNoStoredHashes = errors.New("container has no stored hashes")

// ContainerVerification is the outcome of VerifyContainer.
type ContainerVerification struct {
	Format string
	image.VerifyResult
	// Result is VerifyResult.Result(), or "cancelled" / "error" when the run
	// did not finish.
	Result string
}

// VerifyContainer hashes the whole image and compares it with the hashes the
// container stores, then appends ONE audit entry "image.verify" (also for a
// cancelled or failed run). It returns ErrNoStoredHashes, and writes nothing,
// when the image does not implement image.Verifier. Parser panics are
// recovered into *filesys.CorruptError. A failed audit append is returned as
// the error (the verification result is still returned).
//
// The image is only read: nothing in the case except the audit entry is written.
func (s *Session) VerifyContainer(ctx context.Context, progress func(done, total int64)) (ContainerVerification, error) {
	v, ok := s.Image.(image.Verifier)
	if !ok {
		return ContainerVerification{}, ErrNoStoredHashes
	}
	res, err := runVerify(ctx, v, s.Image.Size(), progress)
	cv := ContainerVerification{Format: s.Image.Format(), VerifyResult: res}
	switch {
	case err == nil:
		cv.Result = res.Result()
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		cv.Result = "cancelled"
	default:
		cv.Result = "error"
	}
	if aerr := s.auditVerify(cv, err); aerr != nil {
		return cv, errors.Join(err, aerr)
	}
	return cv, err
}

// runVerify calls v.Verify with a panic (the parser reads hostile bytes)
// converted to a *filesys.CorruptError and an empty, unverified result.
func runVerify(ctx context.Context, v image.Verifier, size int64, progress func(done, total int64)) (res image.VerifyResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			res = image.VerifyResult{
				Size: size, BadChunk: -1,
				MD5:  image.HashCheck{Status: image.HashUnverified},
				SHA1: image.HashCheck{Status: image.HashUnverified},
			}
			err = panicError("image container", "Verify", p)
		}
	}()
	return v.Verify(ctx, progress)
}

func hashDetails(h image.HashCheck) map[string]any {
	return map[string]any{"stored": h.Stored, "computed": h.Computed, "status": string(h.Status)}
}

// auditVerify appends the image.verify entry. The values are the strings the
// verifier produced; nothing is recomputed here.
func (s *Session) auditVerify(cv ContainerVerification, verr error) error {
	segs := make([]any, len(s.Segments))
	for i, seg := range s.Segments {
		segs[i] = map[string]any{"id": seg.ID, "sha256": seg.SHA256}
	}
	d := map[string]any{
		"parent_id": s.Parent.ID, "parent_path": s.Parent.Path, "parent_sha256": s.Parent.SHA256,
		"segments": segs, "format": cv.Format, "size": cv.Size, "bytes_hashed": cv.BytesHashed,
		"md5": hashDetails(cv.MD5), "sha1": hashDetails(cv.SHA1), "result": cv.Result,
	}
	if cv.BadChunk >= 0 {
		d["first_bad_chunk"] = cv.BadChunk
	}
	if verr != nil {
		d["error"] = verr.Error()
	}
	_, err := s.Case.Audit.Append("image.verify", s.Parent.Source.DeviceID, d)
	return err
}
