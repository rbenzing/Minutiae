package examine

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/image"
)

// gapMaxProblems is how many acquisition-gap problems are listed; the rest is one line.
const gapMaxProblems = 50

// GapCheck is R10: every extract, unallocated and carve artifact's acquisition-gap note equals the note recomputed from
// its parent container's error2 list and its recorded runs, through the same pure function the writers use
// (gapNoteFor) with the size input the writer used. No option disables it.
func GapCheck() evidence.VerifyCheck {
	return verifyGaps
}

// gapArtifactHook sees the id of each artifact at the start of its check (a test seam).
var gapArtifactHook func(id string)

type gapper struct {
	c       *evidence.Case
	rep     *evidence.VerifyReport
	byID    map[string]evidence.ManifestRecord
	listed  int
	dropped int
	add     func(n int64)

	unrecorded      int // incomplete extracts whose kept-byte runs are not recorded (K21 notice)
	unrecordedFirst string
	legacy          int // legacy extracts without a note whose recompute finds an overlap (K29 notice)
	legacyFirst     string
}

func (g *gapper) problem(rec evidence.ManifestRecord, format string, args ...any) {
	if g.listed >= gapMaxProblems {
		g.dropped++
		return
	}
	g.listed++
	g.rep.AddProblem("acquisition gaps: %s (artifact %q, %q)", fmt.Sprintf(format, args...), rec.ID, rec.Path)
}

func verifyGaps(ctx context.Context, c *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport) {
	rep.MarkGapsRan()
	g := &gapper{c: c, rep: rep, byID: make(map[string]evidence.ManifestRecord, len(recs)), add: func(int64) {}}
	byParent := map[string][]evidence.ManifestRecord{}
	var total int64
	for _, rec := range recs {
		if _, dup := g.byID[rec.ID]; dup {
			continue // a duplicate id is reported once by the manifest checks
		}
		g.byID[rec.ID] = rec
		if k := rec.Source.Kind; (k == "extract" || k == "unallocated" || (k == "carve" && !carveOnArtifact(rec))) && rec.Source.Derived != nil {
			p := rec.Source.Derived.ParentID
			byParent[p] = append(byParent[p], rec)
			total++
		}
	}
	done, add := c.VerifyPhase("gaps", total, "artifacts")
	defer done()
	g.add = add
	parents := make([]string, 0, len(byParent))
	for p := range byParent {
		parents = append(parents, p)
	}
	slices.Sort(parents)
	for _, p := range parents {
		if ctx.Err() != nil {
			break
		}
		list := byParent[p]
		slices.SortFunc(list, func(x, y evidence.ManifestRecord) int {
			if c := strings.Compare(x.Path, y.Path); c != 0 {
				return c
			}
			return strings.Compare(x.ID, y.ID)
		})
		if !g.parent(ctx, p, list) {
			break
		}
	}
	if g.dropped > 0 {
		rep.AddProblem("acquisition gaps: %d further problems are not listed", g.dropped)
	}
	if g.unrecorded > 0 {
		rep.AddNotice("acquisition gaps: not recomputable: the runs of the kept bytes are not recorded (%d artifact(s), first %q)", g.unrecorded, g.unrecordedFirst)
	}
	if g.legacy > 0 {
		rep.AddNotice("acquisition gaps: gap note not recomputable (recorded before gap notes) (%d artifact(s), first %q)", g.legacy, g.legacyFirst)
	}
}

// parent rechecks the artifacts derived from one parent container. It returns false when the context ended;
// the findings made so far are kept.
func (g *gapper) parent(ctx context.Context, id string, list []evidence.ManifestRecord) bool {
	s, err := OpenContainer(g.c, id, Options{})
	if err != nil {
		g.problem(list[0], "parent %q cannot be opened: %v (%d artifact(s) not recomputed)", id, err, len(list))
		g.add(int64(len(list)))
		return true
	}
	defer func() { _ = s.Close() }()
	var gaps image.AcquisitionGaps
	reporter := false
	if gr, ok := s.Image.(image.GapReporter); ok {
		gaps, reporter = gr.AcquisitionGaps(), true
	}
	for _, rec := range list {
		if gapArtifactHook != nil {
			gapArtifactHook(rec.ID)
		}
		if ctx.Err() != nil {
			return false
		}
		g.one(rec, s.Image.Format(), gaps, reporter)
		g.add(1)
	}
	return true
}

// declaresGaps reports whether the container declares any acquisition gap or may have lost them.
func declaresGaps(g image.AcquisitionGaps, reporter bool) bool {
	return reporter && (g.Ranges != 0 || g.State == image.GapsDamaged)
}

func (g *gapper) one(rec evidence.ManifestRecord, format string, gaps image.AcquisitionGaps, reporter bool) {
	d := rec.Source.Derived
	recorded := d.AcquisitionGaps
	if !declaresGaps(gaps, reporter) {
		g.rep.GapNotesRecomputed++
		if recorded != nil {
			g.problem(rec, "the %s parent records no acquisition errors, but the artifact carries a gap note", format)
		}
		return
	}
	runs, err := g.c.DerivedRuns(rec, g.byID)
	if err != nil {
		g.problem(rec, "runs unreadable: %v", err)
		return
	}
	runsKnown := len(runs) > 0
	size := gapSize(rec, runs)
	want := gapNoteFor(gaps, runs, runsKnown, size)
	if rec.Incomplete && len(d.Runs) == 0 && d.RunsArtifact == "" && !reflect.DeepEqual(want, recorded) {
		// The kept bytes' runs were never recorded (all sidecar names taken): the note the writer computed from
		// them cannot be recomputed. Not a problem, never silent.
		if g.unrecorded++; g.unrecorded == 1 {
			g.unrecordedFirst = rec.Path
		}
		return
	}
	if rec.Source.Kind == "extract" && d.DeclaredSize == 0 && recorded == nil && want != nil {
		// recorded before gap notes (decided from the recorded structure only)
		if g.legacy++; g.legacy == 1 {
			g.legacyFirst = rec.Path
		}
		return
	}
	g.rep.GapNotesRecomputed++
	if !reflect.DeepEqual(want, recorded) {
		g.problem(rec, "the artifact records %s, recomputed from the container and its runs: %s", renderGapNote(recorded), renderGapNote(want))
	}
}

// gapSize is the size input the writer used (K23): an unallocated export's and a carve's size is the sum of its
// runs (a partial carve's runs are its kept prefix); an extract's is the source file's full size, recorded as
// declared_size. An incomplete extract cut at an unreadable chunk was computed over its kept bytes (its runs sum to
// its size). An extract recorded before declared_size is recomputed with the larger of its runs' length and its size.
func gapSize(rec evidence.ManifestRecord, runs []evidence.Run) int64 {
	sum := runLengths(runs)
	d := rec.Source.Derived
	switch {
	case rec.Source.Kind == "unallocated" || rec.Source.Kind == "carve":
		return sum
	case rec.Incomplete && sum == rec.Size:
		return rec.Size
	case d.DeclaredSize > 0:
		return d.DeclaredSize
	}
	return max(sum, rec.Size)
}

// runLengths sums the lengths of all runs, holes included, saturating.
func runLengths(runs []evidence.Run) int64 {
	var sum int64
	for _, r := range runs {
		if r.Length <= 0 {
			continue
		}
		var ok bool
		if sum, ok = filesys.AddOK(sum, r.Length); !ok {
			return math.MaxInt64
		}
	}
	return sum
}

func renderGapNote(n *evidence.AcquisitionGapNote) string {
	if n == nil {
		return "none"
	}
	return fmt.Sprintf("bytes=%d ranges=%d unknown=%t first=%v", n.Bytes, n.Ranges, n.Unknown, n.First)
}

// carveOnArtifact reports whether rec is a carve of scope artifact: its parent is a case artifact, not a
// container, so it carries no acquisition-gap note and R10 has nothing to recompute (the shape rule refuses one).
func carveOnArtifact(rec evidence.ManifestRecord) bool {
	d := rec.Source.Derived
	return d != nil && d.Recovery != nil && d.Recovery.Scope == CarveArtifact
}
