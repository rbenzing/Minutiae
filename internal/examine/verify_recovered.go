package examine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// reproduceChunk is the most bytes the reproduce check reads from the image or from an artifact at once.
const reproduceChunk = 1 << 20

// reproduceMaxProblems is how many reproduce problems are listed; the rest is one line.
const reproduceMaxProblems = 50

// reproduceReadObserver sees the size of every read the reproduce check makes (a test seam).
var reproduceReadObserver func(n int)

// RecoveredCheck is the byte-level reproduction check (R6) composed into case verify. It has no skip
// option: a recovered artifact is believed only when the bytes of the image at its recorded runs equal it.
func RecoveredCheck() evidence.VerifyCheck { return VerifyRecovered }

// VerifyRecovered reproduces every recover and carve artifact from its parent: the parent is opened as a
// container only (never its partition table, so a damaged table cannot hide a forgery) or, for a carve of
// scope artifact, read as one raw run of the parent artifact; every run must lie inside the parent; and
// the first Size bytes of the runs, read in chunks of at most 1 MiB, must equal the artifact. Failures are
// problems, never aborts; nothing is written to the case. A cancelled ctx stops it at the next chunk.
func VerifyRecovered(ctx context.Context, c *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport) {
	rep.MarkReproduceRan()
	r := &reproducer{ctx: ctx, c: c, rep: rep, byID: make(map[string]evidence.ManifestRecord, len(recs))}
	type groupKey struct {
		parent   string
		artifact bool // a carve of scope "artifact": the parent is read as one raw artifact
	}
	groups := map[groupKey][]evidence.ManifestRecord{}
	for _, rec := range recs {
		if _, dup := r.byID[rec.ID]; dup {
			continue // a duplicate id is reported once by the manifest checks
		}
		r.byID[rec.ID] = rec
	}
	for _, rec := range r.byID {
		d := rec.Source.Derived
		if d == nil || (rec.Source.Kind != evidence.KindRecover && rec.Source.Kind != evidence.KindCarve) {
			continue
		}
		k := groupKey{d.ParentID, rec.Source.Kind == evidence.KindCarve && d.Recovery != nil && d.Recovery.Scope == "artifact"}
		groups[k] = append(groups[k], rec)
	}
	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].parent != keys[j].parent {
			return keys[i].parent < keys[j].parent
		}
		return !keys[i].artifact && keys[j].artifact
	})
	for _, k := range keys {
		if ctx.Err() != nil {
			return
		}
		g := groups[k]
		slices.SortFunc(g, func(a, b evidence.ManifestRecord) int { return strings.Compare(a.Path, b.Path) })
		if !r.group(k.parent, k.artifact, g) {
			return
		}
	}
	if r.dropped > 0 {
		rep.AddProblem("reproduce: %d further problems are not listed", r.dropped)
	}
}

type reproducer struct {
	ctx     context.Context
	c       *evidence.Case
	rep     *evidence.VerifyReport
	byID    map[string]evidence.ManifestRecord
	listed  int
	dropped int
}

func (r *reproducer) problem(rec evidence.ManifestRecord, format string, a ...any) {
	if r.listed >= reproduceMaxProblems {
		r.dropped++
		return
	}
	r.listed++
	r.rep.AddProblem("reproduce: %s (artifact %q, %q)", fmt.Sprintf(format, a...), rec.ID, rec.Path)
}

// group reproduces the artifacts derived from one parent, opening the parent once. It returns false when
// the context ended.
func (r *reproducer) group(parentID string, asArtifact bool, recs []evidence.ManifestRecord) bool {
	var (
		rd       io.ReaderAt
		size     int64
		wantSegs []evidence.SegmentRef
	)
	if asArtifact {
		f, prec, err := r.c.OpenArtifact(parentID)
		if err != nil {
			r.problem(recs[0], "parent %q cannot be opened: %v (%d artifact(s) not checked)", parentID, err, len(recs))
			return true
		}
		defer func() { _ = f.Close() }()
		rd, size = f, prec.Size
	} else {
		s, err := OpenContainer(r.c, parentID, Options{})
		if err != nil {
			r.problem(recs[0], "parent %q cannot be opened: %v (%d artifact(s) not checked)", parentID, err, len(recs))
			return true
		}
		defer func() { _ = s.Close() }()
		rd, size, wantSegs = s.Image, s.Image.Size(), s.ParentSegments()
	}
	for _, rec := range recs {
		if r.ctx.Err() != nil {
			return false
		}
		if !r.one(rec, rd, size, wantSegs) {
			return false
		}
	}
	return true
}

// one reproduces one artifact; false means the context ended.
func (r *reproducer) one(rec evidence.ManifestRecord, rd io.ReaderAt, size int64, wantSegs []evidence.SegmentRef) bool {
	d := rec.Source.Derived
	if !slices.Equal(d.ParentSegments, wantSegs) {
		r.problem(rec, "parent segments differ: the artifact records %d, the opened parent has %d", len(d.ParentSegments), len(wantSegs))
		return true
	}
	runs, err := r.c.DerivedRuns(rec, r.byID)
	if err != nil {
		r.problem(rec, "runs unreadable: %v", err)
		return true
	}
	if len(runs) > evidence.MaxRecoveredRuns {
		return true // too many runs: reported by the runs check
	}
	invalid := false
	outside := false
	for i, run := range runs {
		switch {
		case run.Offset < 0 || run.Length <= 0:
			invalid = true // holes, empty and negative runs are reported by the runs check
		case run.Offset > size || run.Length > size-run.Offset:
			outside = true
			r.problem(rec, "run %d (%d+%d) lies outside the %d-byte parent image", i, run.Offset, run.Length, size)
		}
	}
	if invalid || outside {
		return true
	}
	if len(runs) == 0 && rec.Size > 0 {
		return true // "records no runs" is reported by the runs check
	}
	f, arec, err := r.c.OpenArtifact(rec.ID)
	if err != nil {
		r.problem(rec, "the artifact cannot be opened: %v", err)
		return true
	}
	defer func() { _ = f.Close() }()
	remaining := arec.Size
	var pos int64
	ibuf := make([]byte, min(reproduceChunk, max(arec.Size, 1)))
	abuf := make([]byte, len(ibuf))
	for _, run := range runs {
		for off := int64(0); off < run.Length && remaining > 0; {
			if r.ctx.Err() != nil {
				return false
			}
			n := int(min(int64(len(ibuf)), run.Length-off, remaining))
			at := run.Offset + off
			if got, err := readFull(rd, ibuf[:n], at); err != nil {
				r.problem(rec, "parent image unreadable at offset %d: %v", at+int64(got), err)
				return true
			}
			if got, err := readFull(f, abuf[:n], pos); err != nil {
				r.problem(rec, "the artifact is unreadable at byte %d: %v", pos+int64(got), err)
				return true
			}
			if !bytes.Equal(ibuf[:n], abuf[:n]) {
				i := 0
				for ibuf[i] == abuf[i] {
					i++
				}
				r.problem(rec, "byte %d of the artifact differs from image offset %d", pos+int64(i), at+int64(i))
				return true
			}
			pos += int64(n)
			off += int64(n)
			remaining -= int64(n)
		}
	}
	if remaining == 0 {
		r.rep.RecoveredReproduced++
	}
	return true
}

// readFull fills p from rd at off. It returns the bytes read and an error unless p is full (an io.EOF that
// arrives with the last bytes is not one).
func readFull(rd io.ReaderAt, p []byte, off int64) (int, error) {
	if reproduceReadObserver != nil {
		reproduceReadObserver(len(p))
	}
	n, err := rd.ReadAt(p, off)
	if n == len(p) {
		return n, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
