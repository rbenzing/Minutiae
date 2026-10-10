package evidence

import "path"

// declaredRefs counts, over the whole case, how many extract artifacts name each
// declared runs sidecar.
func declaredRefs(recs []ManifestRecord) map[string]int {
	n := map[string]int{}
	for _, r := range recs {
		if d := r.Source.Derived; d != nil && d.DeclaredRunsArtifact != "" {
			n[d.DeclaredRunsArtifact]++
		}
	}
	return n
}

// checkExtractDeclared checks Derivation.DeclaredRunsArtifact: only an incomplete
// extract may carry it, it must name a runs sidecar of the artifact's own parent
// in its own directory, other than its captured-runs sidecar, and no other
// artifact may name the same sidecar. When the link is structurally sound, the
// artifact's captured runs are recomputed against the declared list: they must
// be an in-order prefix of it (same offsets and lengths, the last captured run
// may be a shorter cut).
func (c *Case) checkExtractDeclared(ps *problemSet, r ManifestRecord, pre string, byID map[string]ManifestRecord, refs map[string]int) {
	d := r.Source.Derived
	if d == nil || d.DeclaredRunsArtifact == "" {
		return
	}
	id := d.DeclaredRunsArtifact
	if r.Source.Kind != "extract" {
		ps.add(rKindNamespace, "%sdeclared runs artifact on kind %q (only an extract takes one; recovered kinds use the recovery description)", pre, r.Source.Kind)
		return
	}
	sound := true
	if !r.Incomplete {
		ps.add(rKindNamespace, "%sdeclared runs artifact %q on a complete artifact (only an incomplete extract has an unread declared list)", pre, id)
	}
	sc, ok := byID[id]
	switch {
	case !ok:
		ps.add(rKindNamespace, "%sdeclared runs artifact %q is not in the manifest", pre, id)
		return
	case sc.Source.Kind != "runs" || path.Dir(sc.Path) != path.Dir(r.Path):
		ps.add(rKindNamespace, "%sdeclared runs artifact %q is not a runs sidecar in the same directory", pre, id)
		sound = false
	case sc.Source.Derived == nil || sc.Source.Derived.ParentID != d.ParentID:
		ps.add(rKindNamespace, "%sdeclared runs artifact %q is not a runs sidecar of the artifact's parent", pre, id)
		sound = false
	}
	if id == d.RunsArtifact {
		ps.add(rKindNamespace, "%sdeclared runs artifact %q is also the captured runs sidecar", pre, id)
		sound = false
	}
	if refs[id] > 1 {
		ps.add(rKindNamespace, "%sdeclared runs artifact %q is referenced by more than one artifact", pre, id)
	}
	if sound {
		c.checkCapturedIsPrefix(ps, r, pre, byID)
	}
}

// checkCapturedIsPrefix recomputes that r's captured runs are an in-order prefix
// of the declared list it links to. An unreadable list or captured set is a
// problem, never a skipped check.
func (c *Case) checkCapturedIsPrefix(ps *problemSet, r ManifestRecord, pre string, byID map[string]ManifestRecord) {
	d := r.Source.Derived
	captured, err := c.DerivedRuns(r, byID)
	if err != nil {
		ps.add(rKindNamespace, "%scannot compare the captured runs with the declared list: %v", pre, err)
		return
	}
	if len(captured) == 0 {
		return // nothing captured: the empty prefix
	}
	full, err := c.DerivedRuns(ManifestRecord{ID: r.ID, Path: r.Path, Source: Source{Kind: "extract", Derived: &Derivation{ParentID: d.ParentID, RunsArtifact: d.DeclaredRunsArtifact}}}, byID)
	if err != nil {
		ps.add(rKindNamespace, "%scannot compare the captured runs with the declared list: %v", pre, err)
		return
	}
	if len(captured) > len(full) {
		ps.add(rKindNamespace, "%scaptured runs (%d) are not a prefix of the declared run list %q (%d runs)", pre, len(captured), d.DeclaredRunsArtifact, len(full))
		return
	}
	last := len(captured) - 1
	for i, cr := range captured {
		fr := full[i]
		same := cr.Offset == fr.Offset && cr.Length == fr.Length
		if i == last {
			same = cr.Offset == fr.Offset && cr.Length > 0 && cr.Length <= fr.Length
		}
		if !same {
			ps.add(rKindNamespace, "%scaptured run %d (offset %d, length %d) is not a prefix of the declared run list %q (offset %d, length %d)", pre, i+1, cr.Offset, cr.Length, d.DeclaredRunsArtifact, fr.Offset, fr.Length)
			return
		}
	}
}
