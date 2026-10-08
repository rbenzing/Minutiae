package dbrecover

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// sealedInput is one input of a run: opened read-only through the case and
// hashed against the manifest before anything parses it.
type sealedInput struct {
	Ref inputRef
	Rec evidence.ManifestRecord
	f   *os.File
}

// sealedSet is the database and its companions.
type sealedSet struct {
	DB           sealedInput
	WAL, Journal *sealedInput
	man          map[string]evidence.ManifestRecord
	// openArtifact is the case's OpenArtifact; checkUnchanged re-opens through it.
	// A test seam: only export-free test code replaces it.
	openArtifact func(id string) (*os.File, evidence.ManifestRecord, error)
}

// eligibleKind says whether an artifact of this manifest record may be a
// database input, with the reason when it may not.
func eligibleKind(rec evidence.ManifestRecord) (bool, string) {
	switch rec.Source.Kind {
	case "file", "backup", "extract", evidence.KindRecover, evidence.KindCarve:
		return true, ""
	}
	return false, fmt.Sprintf("an artifact of kind %q is not database input (only file, backup, extract, recover and carve are)", rec.Source.Kind)
}

// ctxReader fails a read once ctx is done, so a long hash can be cancelled.
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

func hashReader(ctx context.Context, r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, r}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// freshCheck hashes the held reader and a fresh re-open and requires both to
// equal want. A read or open error is returned as it is (it says nothing about
// the evidence); a different digest is an *InputChangedError.
func freshCheck(ctx context.Context, id, want string, held io.Reader, reopen func() (io.ReadCloser, error)) error {
	got, err := hashReader(ctx, held)
	if err != nil {
		return fmt.Errorf("re-hash of input %s (held descriptor): %w", id, err)
	}
	if got != want {
		return &InputChangedError{ArtifactID: id, Want: want, Got: got}
	}
	rc, err := reopen()
	if err != nil {
		return fmt.Errorf("re-open of input %s: %w", id, err)
	}
	got, err = hashReader(ctx, rc)
	cerr := rc.Close()
	if err != nil {
		return fmt.Errorf("re-hash of input %s (re-open): %w", id, err)
	}
	if got != want {
		return &InputChangedError{ArtifactID: id, Want: want, Got: got}
	}
	if cerr != nil {
		return fmt.Errorf("close of input %s: %w", id, cerr)
	}
	return nil
}

// openInputs opens the database and the named companions (an empty id is
// absent) read-only through the case, checks each against the manifest
// snapshot man and hashes it before returning. Any failure closes what was
// opened.
func openInputs(ctx context.Context, c *evidence.Case, man []evidence.ManifestRecord, db, wal, journal string) (*sealedSet, error) {
	s := &sealedSet{man: make(map[string]evidence.ManifestRecord, len(man)), openArtifact: c.OpenArtifact}
	seen := map[string]string{}
	for _, x := range []struct{ role, id string }{{"db", db}, {"wal", wal}, {"journal", journal}} {
		if x.id == "" {
			continue
		}
		if prev, dup := seen[x.id]; dup {
			return nil, &RefusalError{Reason: ReasonBadOption, Detail: fmt.Sprintf("artifact %s is given as both the %s and the %s input", x.id, prev, x.role)}
		}
		seen[x.id] = x.role
	}
	for _, r := range man {
		s.man[r.ID] = r
	}
	d, err := s.open(ctx, "db", db)
	if err != nil {
		return nil, err
	}
	s.DB = *d
	for _, x := range []struct {
		role, id string
		dst      **sealedInput
	}{{"wal", wal, &s.WAL}, {"journal", journal, &s.Journal}} {
		if x.id == "" {
			continue
		}
		in, err := s.open(ctx, x.role, x.id)
		if err != nil {
			s.close()
			return nil, err
		}
		*x.dst = in
	}
	return s, nil
}

func (s *sealedSet) open(ctx context.Context, role, id string) (*sealedInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rec, ok := s.man[id]
	if !ok {
		return nil, fmt.Errorf("%s input: %w: %q", role, evidence.ErrUnknownArtifact, id)
	}
	if ok, why := eligibleKind(rec); !ok {
		return nil, &RefusalError{Reason: ReasonIneligibleKind, Detail: fmt.Sprintf("%s input %s: %s", role, id, why)}
	}
	f, got, err := s.openArtifact(id)
	if err != nil {
		return nil, fmt.Errorf("%s input %s: %w", role, id, err)
	}
	if got.SHA256 != rec.SHA256 || got.Size != rec.Size {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s input %s differs from the manifest snapshot", evidence.ErrIntegrity, role, id)
	}
	sum, err := hashReader(ctx, io.NewSectionReader(f, 0, math.MaxInt64))
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("hash of %s input %s: %w", role, id, err)
	}
	if sum != rec.SHA256 {
		_ = f.Close()
		return nil, &InputChangedError{ArtifactID: id, Want: rec.SHA256, Got: sum}
	}
	in := &sealedInput{Rec: rec, f: f, Ref: inputRef{
		Role: role, ArtifactID: id, SHA256: rec.SHA256, Size: rec.Size, Incomplete: rec.Incomplete,
	}}
	if tr, found := evidence.RecoveryTrailOf(s.man, id); found {
		in.Ref.Recovered = true
		in.Ref.Cap = tr.MinConfidence
	}
	return in, nil
}

func (s *sealedSet) inputs() []*sealedInput {
	out := []*sealedInput{&s.DB}
	if s.WAL != nil {
		out = append(out, s.WAL)
	}
	if s.Journal != nil {
		out = append(out, s.Journal)
	}
	return out
}

// checkUnchanged hashes every input afresh, from the held descriptor and from
// a re-open through the case (so a replaced file is seen as well as changed
// bytes), and requires the manifest digest. A mismatch is an
// *InputChangedError; an I/O error is returned as it is.
func (s *sealedSet) checkUnchanged(ctx context.Context) error {
	for _, in := range s.inputs() {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := in.Ref.ArtifactID
		err := freshCheck(ctx, id, in.Rec.SHA256, io.NewSectionReader(in.f, 0, math.MaxInt64), func() (io.ReadCloser, error) {
			f, _, err := s.openArtifact(id)
			if err != nil {
				return nil, err
			}
			return f, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *sealedSet) close() {
	for _, in := range s.inputs() {
		if in.f != nil {
			_ = in.f.Close()
		}
	}
}

// cap is the lowest confidence ceiling over the whole input set (D2), nil when
// no input is recovered.
func (s *sealedSet) cap() *int {
	var m *int
	for _, in := range s.inputs() {
		if c := in.Ref.Cap; c != nil && (m == nil || *c < *m) {
			v := *c
			m = &v
		}
	}
	return m
}

// mixed reports whether some inputs are recovered and others live.
func (s *sealedSet) mixed() bool {
	in := s.inputs()
	for _, x := range in[1:] {
		if x.Ref.Recovered != in[0].Ref.Recovered {
			return true
		}
	}
	return false
}

// incomplete reports whether any input is flagged incomplete.
func (s *sealedSet) incomplete() bool {
	for _, in := range s.inputs() {
		if in.Ref.Incomplete {
			return true
		}
	}
	return false
}
