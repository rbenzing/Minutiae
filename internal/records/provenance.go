package records

import (
	"fmt"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// LinkState says how a chain hop was reached or why the walk stopped at it.
// A hop reached cleanly is LinkRoot (it has no derivation) or LinkOK (it has
// one and the walk continues); a parent whose recorded hash differs from the
// child's ParentSHA256 is appended flagged LinkHashDiffers; a hop whose own
// parent cannot be followed carries the reason the walk stopped there.
type LinkState string

// The link states of a Hop.
const (
	LinkRoot            LinkState = "root"
	LinkOK              LinkState = "ok"
	LinkParentMissing   LinkState = "parent-missing"
	LinkParentAmbiguous LinkState = "parent-ambiguous"
	LinkHashDiffers     LinkState = "parent-hash-differs"
	LinkCycle           LinkState = "cycle"
	LinkTooDeep         LinkState = "too-deep"
)

// SegmentCheck is one parent segment and what the manifest says about it:
// "ok", "missing", "ambiguous" or "hash-differs".
type SegmentCheck struct {
	evidence.SegmentRef
	State string
}

// Hop is one artifact of a provenance chain.
type Hop struct {
	Artifact      evidence.ManifestRecord
	Link          LinkState
	Segments      []SegmentCheck // the first MaxShownSegments parent segments, checked
	SegmentsTotal int            // all parent segments the artifact names
	SegmentsBad   int            // how many of them are not "ok"
	Audit         evidence.AuditBinding
}

// ProblemKind names the kind of a provenance Problem.
type ProblemKind string

// The problem kinds.
const (
	ProblemParentMissing    ProblemKind = "parent-missing"
	ProblemParentAmbiguous  ProblemKind = "parent-ambiguous"
	ProblemParentHash       ProblemKind = "parent-hash"
	ProblemCycle            ProblemKind = "cycle"
	ProblemTooDeep          ProblemKind = "too-deep"
	ProblemSegment          ProblemKind = "segment"
	ProblemAuditMissing     ProblemKind = "audit-missing"
	ProblemAuditDiffers     ProblemKind = "audit-differs"
	ProblemAuditDuplicate   ProblemKind = "audit-duplicate"
	ProblemAuditUnreadable  ProblemKind = "audit-unreadable"
	ProblemRunsSidecar      ProblemKind = "runs-sidecar"
	ProblemRunsInconsistent ProblemKind = "runs-inconsistent"
	ProblemRangeOutside     ProblemKind = "range-outside"
	ProblemRecoveryLive     ProblemKind = "recovery-live-record"
	ProblemRecoveryConf     ProblemKind = "recovery-confidence"
	ProblemDeclaredRuns     ProblemKind = "declared-runs"
)

// Problem is one thing wrong with a provenance chain. Hop is the chain index
// it concerns, -1 for the record itself.
type Problem struct {
	Kind   ProblemKind
	Hop    int
	Detail string
}

// OffsetState says whether a record's byte range could be placed in the image.
type OffsetState string

// The offset states.
const (
	OffsetNoRange     OffsetState = "no-range"
	OffsetTranslated  OffsetState = "translated"
	OffsetUnavailable OffsetState = "unavailable"
)

// OffsetHop is the translation of the range through one derivation hop.
type OffsetHop struct {
	From, To, Runs string
	Extents        []ImageExtent
	Total          int
	Truncated      bool
	RunsExceedSize bool
}

// OffsetInfo is the image-offset part of a Provenance.
type OffsetInfo struct {
	State       OffsetState
	Reason      string // never empty when State is OffsetUnavailable
	Coordinates string // always "media"
	Segments    int    // segments of the root image (0 = unknown or not an import)
	Hops        []OffsetHop
	Image       []ImageExtent // the last hop's extents when translated and the chain reached a root
}

// DeclaredRunsView describes the declared-runs sidecar of an interrupted copy.
// State: ok, missing, not-runs-sidecar, other-parent, other-directory,
// multiply-referenced or used-as-captured.
type DeclaredRunsView struct{ ArtifactID, State, Detail string }

// RecoveryView is the recovery description found along a chain.
type RecoveryView struct {
	ArtifactID    string
	Hops          int
	Recovery      evidence.Recovery
	MinConfidence *int
	DeclaredRuns  *DeclaredRunsView
}

// Provenance is the resolved origin of a record's artifact.
type Provenance struct {
	Chain              []Hop
	ReachedRoot        bool
	Recovery           *RecoveryView
	Offset             OffsetInfo
	Notes              []string
	Problems           []Problem
	ProblemsSuppressed int
}

// OK reports a provenance with no problem, listed or suppressed.
func (p Provenance) OK() bool { return len(p.Problems) == 0 && p.ProblemsSuppressed == 0 }

// Provenance limits.
const (
	MaxProblemsPerKind = 50
	MaxShownSegments   = 20
)

// problemSet collects problems, listing at most MaxProblemsPerKind per kind
// and counting the rest.
type problemSet struct {
	list       []Problem
	perKind    map[ProblemKind]int
	suppressed int
}

// full reports whether kind is at its cap, so a caller can skip formatting a
// detail that would be dropped.
func (s *problemSet) full(kind ProblemKind) bool { return s.perKind[kind] >= MaxProblemsPerKind }

func (s *problemSet) add(kind ProblemKind, hop int, detail string) {
	if s.full(kind) {
		s.suppressed++
		return
	}
	s.perKind[kind]++
	s.list = append(s.list, Problem{Kind: kind, Hop: hop, Detail: detail})
}

// resolveChain walks the derivation chain of artifactID through man. It is
// pure: no I/O, no clock. The audit index must have been built from a verified
// audit log (Bind is a consistency check only); duplicate manifest ids, which
// Bind does not see, are found here through the id -> positions map.
func resolveChain(man []evidence.ManifestRecord, audit *evidence.AuditIndex, artifactID string) Provenance {
	if audit == nil {
		audit = evidence.NewAuditIndex(nil)
	}
	pos := make(map[string][]int, len(man))
	for i := range man {
		pos[man[i].ID] = append(pos[man[i].ID], i)
	}
	ps := &problemSet{perKind: map[ProblemKind]int{}}
	var p Provenance
	p.Offset = OffsetInfo{Coordinates: "media"}

	finish := func() Provenance {
		p.Problems = ps.list
		p.ProblemsSuppressed = ps.suppressed
		return p
	}

	switch len(pos[artifactID]) {
	case 0:
		ps.add(ProblemParentMissing, -1, fmt.Sprintf("artifact %q is not in the manifest", artifactID))
		return finish()
	case 1:
	default:
		ps.add(ProblemParentAmbiguous, -1, fmt.Sprintf("artifact id %q is held by %d manifest records", artifactID, len(pos[artifactID])))
		return finish()
	}

	inChain := map[string]bool{}
	cur := man[pos[artifactID][0]]
	link := LinkOK
	for {
		hop := len(p.Chain)
		inChain[cur.ID] = true
		h := Hop{Artifact: cur, Audit: audit.Bind(cur)}
		if h.Audit.State != evidence.AuditBound {
			ps.add(ProblemKind("audit-"+string(h.Audit.State)), hop, auditDetail(cur.ID, h.Audit))
		}
		d := cur.Source.Derived
		if d == nil {
			if link != LinkHashDiffers {
				link = LinkRoot
			}
			h.Link = link
			p.Chain = append(p.Chain, h)
			p.ReachedRoot = true
			p.Offset.Segments = cur.Source.Segments
			return finish()
		}
		checkSegments(&h, d.ParentSegments, pos, man, ps, hop)
		h.Link = link
		parent, state, detail := followParent(d, pos, man, inChain)
		if state == "" && len(p.Chain)+1 > evidence.MaxDerivedDepth {
			state, detail = LinkTooDeep, fmt.Sprintf("the chain has more than %d links", evidence.MaxDerivedDepth)
		}
		p.Chain = append(p.Chain, h)
		if state != "" {
			p.Chain[hop].Link = state
			ps.add(stopKind(state), hop, detail)
			return finish()
		}
		link = LinkOK
		if parent.SHA256 != d.ParentSHA256 {
			link = LinkHashDiffers
			ps.add(ProblemParentHash, hop+1, fmt.Sprintf("parent %q has sha256 %q but %q records %q", parent.ID, parent.SHA256, cur.ID, d.ParentSHA256))
		}
		cur = parent
	}
}

func auditDetail(id string, b evidence.AuditBinding) string {
	if b.Detail != "" {
		return fmt.Sprintf("artifact %q: %s (%s)", id, b.State, b.Detail)
	}
	return fmt.Sprintf("artifact %q: %s", id, b.State)
}

// followParent looks up the parent of a derivation. A non-empty state means the
// walk cannot continue.
func followParent(d *evidence.Derivation, pos map[string][]int, man []evidence.ManifestRecord, inChain map[string]bool) (evidence.ManifestRecord, LinkState, string) {
	ids := pos[d.ParentID]
	switch {
	case len(ids) == 0:
		return evidence.ManifestRecord{}, LinkParentMissing, fmt.Sprintf("parent %q is not in the manifest", d.ParentID)
	case len(ids) > 1:
		return evidence.ManifestRecord{}, LinkParentAmbiguous, fmt.Sprintf("parent id %q is held by %d manifest records", d.ParentID, len(ids))
	case inChain[d.ParentID]:
		return evidence.ManifestRecord{}, LinkCycle, fmt.Sprintf("parent %q is already in the chain", d.ParentID)
	}
	return man[ids[0]], "", ""
}

func stopKind(s LinkState) ProblemKind {
	switch s {
	case LinkParentMissing:
		return ProblemParentMissing
	case LinkParentAmbiguous:
		return ProblemParentAmbiguous
	case LinkCycle:
		return ProblemCycle
	}
	return ProblemTooDeep
}

// checkSegments checks every parent segment against the manifest, keeping the
// first MaxShownSegments and counting all.
func checkSegments(h *Hop, segs []evidence.SegmentRef, pos map[string][]int, man []evidence.ManifestRecord, ps *problemSet, hop int) {
	h.SegmentsTotal = len(segs)
	for i, s := range segs {
		state := "ok"
		switch ids := pos[s.ID]; {
		case len(ids) == 0:
			state = "missing"
		case len(ids) > 1:
			state = "ambiguous"
		case man[ids[0]].SHA256 != s.SHA256:
			state = "hash-differs"
		}
		if state != "ok" {
			h.SegmentsBad++
			detail := ""
			if !ps.full(ProblemSegment) { // format only what will be listed
				detail = fmt.Sprintf("segment %d (%q): %s", i+1, s.ID, state)
			}
			ps.add(ProblemSegment, hop, detail)
		}
		if i < MaxShownSegments {
			h.Segments = append(h.Segments, SegmentCheck{SegmentRef: s, State: state})
		}
	}
}
