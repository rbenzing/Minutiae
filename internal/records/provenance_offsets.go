package records

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
)

const (
	noteRunsExceedSize = "runs describe more than the bytes held (incomplete)"
	noteNoRuns         = "the derived artifact records no runs (content decoded or runs not recorded)"
	reasonUnproven     = "parent hash differs; offsets refer to unproven bytes"
)

// piece is a stretch of the record's range being followed down the chain: origin is its offset in the
// record's own artifact, start its offset in the artifact of the hop being translated.
type piece struct {
	origin, start, length int64
	hole                  bool
}

// resolveOffsets fills p.Offset: the record's byte range placed in the coordinates of the image at the
// root of the chain, hop by hop, through runs that are proven before they are believed. It never
// guesses: whatever cannot be proven leaves the state Unavailable with the reason, and the hops
// translated so far are kept. Bytes of a hole stay a hole.
func (r *Reader) resolveOffsets(p *Provenance, ix *evidence.ManifestIndex, rng *Range) {
	off := &p.Offset
	off.Coordinates = "media"
	if rng == nil {
		off.State = OffsetNoRange
		return
	}
	unavailable := func(format string, a ...any) {
		off.State, off.Reason = OffsetUnavailable, fmt.Sprintf(format, a...)
	}
	if len(p.Chain) == 0 {
		unavailable("the artifact is not in the manifest")
		return
	}
	// case verify P9, for every record whatever its artifact is: the range lies inside the artifact
	// (checked arithmetic; off+n is never computed)
	if size := p.Chain[0].Artifact.Size; rng.Offset < 0 || rng.Length < 0 || rng.Offset > size || rng.Length > size-rng.Offset {
		reason := fmt.Sprintf("range lies outside the artifact (%d+%d > %d)", rng.Offset, rng.Length, size)
		p.addProblem(ProblemRangeOutside, -1, fmt.Sprintf("artifact %q: %s", p.Chain[0].Artifact.ID, reason))
		unavailable("%s", reason)
		return
	}
	if p.Chain[0].Artifact.Source.Derived == nil {
		unavailable("artifact is not derived")
		return
	}
	pieces := []piece{{origin: rng.Offset, start: rng.Offset, length: rng.Length}}
	exceed := false
	for i := 0; ; i++ {
		hop := p.Chain[i]
		art := hop.Artifact
		d := art.Source.Derived
		if d == nil {
			break // the root image
		}
		if i == len(p.Chain)-1 {
			unavailable("the provenance chain is broken at %s (%s); no image offset", art.ID, hop.Link)
			return
		}
		parent := p.Chain[i+1].Artifact
		oh := OffsetHop{From: art.ID, To: parent.ID, Runs: runsSource(d)}
		if d.Recovery != nil && d.RunsArtifact != "" && d.Recovery.DeclaredRunsArtifact == d.RunsArtifact {
			off.Hops = append(off.Hops, oh)
			unavailable("runs artifact is the declared list, not captured runs")
			return
		}
		runs, err := r.c.CheckedRuns(ix, art)
		switch {
		case errors.Is(err, evidence.ErrNoRuns):
			off.Hops = append(off.Hops, oh)
			p.Notes = append(p.Notes, noteNoRuns)
			if i == 0 {
				unavailable("%s", noteNoRuns)
			} else {
				unavailable("%s (%s)", noteNoRuns, art.ID)
			}
			return
		case errors.Is(err, evidence.ErrIntegrity):
			off.Hops = append(off.Hops, oh)
			p.addProblem(ProblemRunsSidecar, i, fmt.Sprintf("artifact %q: %v", art.ID, err))
			unavailable("the runs of %s are not proven: %v", art.ID, err)
			return
		case err != nil:
			off.Hops = append(off.Hops, oh)
			unavailable("the runs of %s could not be read: %v", art.ID, err)
			return
		}
		if msg := recoveredRunsFault(art, runs); msg != "" {
			// case verify R4: a recovered artifact records exactly the runs of the bytes it holds
			off.Hops = append(off.Hops, oh)
			p.addProblem(ProblemRunsInconsistent, i, fmt.Sprintf("artifact %q: %s", art.ID, msg))
			unavailable("%s", msg)
			return
		}
		var next []piece
		for _, pc := range pieces {
			if pc.hole {
				next = append(next, pc)
				continue
			}
			tr, terr := TranslateRange(runs, art.Size, art.Incomplete, Range{Offset: pc.start, Length: pc.length})
			if terr != nil {
				off.Hops = append(off.Hops, oh)
				kind := ProblemRunsInconsistent
				if i == 0 && errors.Is(terr, errRangeOutside) {
					kind = ProblemRangeOutside
				}
				reason := strings.TrimPrefix(terr.Error(), ErrOffsetUnavailable.Error()+": ")
				p.addProblem(kind, i, fmt.Sprintf("artifact %q: %s", art.ID, reason))
				unavailable("%s", reason)
				return
			}
			oh.Total += tr.Total
			oh.Truncated = oh.Truncated || tr.Truncated
			oh.RunsExceedSize = oh.RunsExceedSize || tr.RunsExceedSize
			for _, e := range tr.Extents {
				if len(oh.Extents) < MaxExtentsPerHop {
					oh.Extents = append(oh.Extents, e)
				}
				next = append(next, piece{origin: pc.origin + (e.ArtifactOffset - pc.start), start: e.ImageOffset, length: e.Length, hole: e.Hole})
			}
		}
		if oh.Total > MaxExtentsPerHop {
			oh.Truncated = true
		}
		off.Hops = append(off.Hops, oh)
		exceed = exceed || oh.RunsExceedSize
		if oh.Truncated {
			if p.Chain[i+1].Artifact.Source.Derived != nil {
				unavailable("more than %d fragments at %s; translation stopped", MaxExtentsPerHop, art.ID)
				return
			}
			if len(next) > MaxExtentsPerHop {
				next = next[:MaxExtentsPerHop]
			}
		}
		pieces = next
	}
	// proof of the chain: a parent whose hash differs, or parent segments that do not check out,
	// make the offsets refer to bytes the case does not hold
	for _, h := range p.Chain {
		if h.Link == LinkHashDiffers || h.SegmentsBad > 0 {
			unavailable("%s", reasonUnproven)
			return
		}
	}
	for _, h := range p.Chain {
		if h.Audit.State != evidence.AuditBound {
			unavailable("artifact %s is not bound to its audit entry (%s); offsets refer to unproven provenance", h.Artifact.ID, h.Audit.State)
			return
		}
	}
	if exceed {
		p.Notes = append(p.Notes, noteRunsExceedSize)
	}
	off.State = OffsetTranslated
	off.Image = make([]ImageExtent, 0, len(pieces))
	for _, pc := range pieces {
		e := ImageExtent{ArtifactOffset: pc.origin, Length: pc.length, ImageOffset: pc.start}
		if pc.hole {
			e.ImageOffset, e.Hole = -1, true
		}
		off.Image = append(off.Image, e)
	}
}

// recoveredRunsFault is rule R4 of case verify for a recovered kind: no hole, and the runs add up
// to the size of the artifact whether or not it is flagged incomplete. It returns "" when the
// artifact is not of a recovered kind or the runs are fine.
func recoveredRunsFault(art evidence.ManifestRecord, runs []evidence.Run) string {
	if !evidence.IsRecoveredKind(art.Source.Kind) {
		return ""
	}
	var sum int64
	for i, ru := range runs {
		if ru.Offset == -1 {
			return fmt.Sprintf("run %d is a hole (recovered artifacts have no holes)", i)
		}
		if ru.Length > 0 && sum <= math.MaxInt64-ru.Length {
			sum += ru.Length
		}
	}
	if sum != art.Size {
		return fmt.Sprintf("runs cover %d bytes but the artifact holds %d", sum, art.Size)
	}
	return ""
}

// runsSource names where the runs of a derivation are held.
func runsSource(d *evidence.Derivation) string {
	switch {
	case d.RunsArtifact != "":
		return "sidecar " + d.RunsArtifact
	case len(d.Runs) > 0:
		return "inline"
	}
	return "none"
}
