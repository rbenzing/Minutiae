package records

import (
	"fmt"
	"path"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// noteRecordLevelRecovery explains a recovered record on a chain that holds no recovered artifact (E7).
const noteRecordLevelRecovery = "record is recovered inside a live artifact (for example rows from a database freelist): " +
	"the artifact itself is live evidence, only this record is recovered data"

// addProblem appends a problem to p, honouring the per-kind cap.
func (p *Provenance) addProblem(kind ProblemKind, hop int, detail string) {
	n := 0
	for _, pr := range p.Problems {
		if pr.Kind == kind {
			n++
		}
	}
	if n >= MaxProblemsPerKind {
		p.ProblemsSuppressed++
		return
	}
	p.Problems = append(p.Problems, Problem{Kind: kind, Hop: hop, Detail: detail})
}

// resolveRecovery fills p.Recovery from the nearest recovery description on the chain of the
// record's artifact and checks the record against it. Recovered data is never presented as live:
// the view is set whenever the chain holds a recovered artifact, whatever else is wrong.
func (r *Reader) resolveRecovery(p *Provenance, row Row, byID map[string]evidence.ManifestRecord) {
	tr, ok := evidence.RecoveryTrailOf(byID, row.ArtifactID)
	if !ok {
		if row.Recovered {
			p.Notes = append(p.Notes, noteRecordLevelRecovery)
		}
		return
	}
	v := &RecoveryView{ArtifactID: tr.ArtifactID, Hops: tr.Hops, Recovery: *tr.Recovery}
	if tr.MinConfidence != nil {
		m := *tr.MinConfidence
		v.MinConfidence = &m
	}
	p.Recovery = v
	switch {
	case !row.Recovered:
		p.addProblem(ProblemRecoveryLive, tr.Hops, fmt.Sprintf("live record (not recovered) on recovered artifact %q (%s)", tr.ArtifactID, tr.Recovery.Class))
	case tr.MinConfidence != nil && row.Confidence == nil:
		p.addProblem(ProblemRecoveryConf, tr.Hops, fmt.Sprintf("recovered record has no confidence; artifact %q is recovered", tr.ArtifactID))
	case tr.MinConfidence != nil && *row.Confidence > *tr.MinConfidence:
		p.addProblem(ProblemRecoveryConf, tr.Hops, fmt.Sprintf("record confidence %d is above the chain's lowest recovery confidence %d", *row.Confidence, *tr.MinConfidence))
	}
	if id := tr.Recovery.DeclaredRunsArtifact; id != "" {
		dv := declaredRunsView(byID, byID[tr.ArtifactID], id)
		v.DeclaredRuns = &dv
		if dv.State != "ok" {
			p.addProblem(ProblemDeclaredRuns, tr.Hops, dv.Detail)
		}
	}
}

// declaredRunsView checks the declared run list of art like checkDeclaredRuns of case verify; the
// list itself is never read.
func declaredRunsView(byID map[string]evidence.ManifestRecord, art evidence.ManifestRecord, id string) DeclaredRunsView {
	v := DeclaredRunsView{ArtifactID: id}
	set := func(state, format string, a ...any) DeclaredRunsView {
		v.State, v.Detail = state, fmt.Sprintf(format, a...)
		return v
	}
	sc, ok := byID[id]
	if !ok {
		return set("missing", "declared runs artifact %q is not in the manifest", id)
	}
	d := art.Source.Derived
	switch {
	case sc.Source.Kind != "runs":
		return set("not-runs-sidecar", "declared runs artifact %q is not a runs sidecar", id)
	case path.Dir(sc.Path) != path.Dir(art.Path):
		return set("other-directory", "declared runs artifact %q is not in the same directory as the artifact", id)
	case sc.Source.Derived == nil || d == nil || sc.Source.Derived.ParentID != d.ParentID:
		return set("other-parent", "declared runs artifact %q is not a runs sidecar of the artifact's parent", id)
	case d.RunsArtifact == id:
		return set("used-as-captured", "declared runs artifact %q is also the artifact's own captured runs", id)
	}
	n := 0
	for _, m := range byID {
		md := m.Source.Derived
		if md == nil || !evidence.IsRecoveredKind(m.Source.Kind) {
			continue
		}
		// captured and declared references both count, as in checkDeclaredRuns of case verify
		if md.RunsArtifact == id {
			n++
		}
		if md.Recovery != nil && md.Recovery.DeclaredRunsArtifact == id {
			n++
		}
	}
	if n > 1 {
		return set("multiply-referenced", "declared runs artifact %q is referenced by %d artifacts", id, n)
	}
	return set("ok", "declared runs artifact %q", id)
}
