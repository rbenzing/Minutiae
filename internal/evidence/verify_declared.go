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
// artifact may name the same sidecar. The link is checked structurally; whether
// the artifact's captured runs are a prefix of the declared list is not.
func checkExtractDeclared(ps *problemSet, r ManifestRecord, pre string, byID map[string]ManifestRecord, refs map[string]int) {
	d := r.Source.Derived
	if d == nil || d.DeclaredRunsArtifact == "" {
		return
	}
	id := d.DeclaredRunsArtifact
	if r.Source.Kind != "extract" {
		ps.add(rKindNamespace, "%sdeclared runs artifact on kind %q (only an extract takes one; recovered kinds use the recovery description)", pre, r.Source.Kind)
		return
	}
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
	case sc.Source.Derived == nil || sc.Source.Derived.ParentID != d.ParentID:
		ps.add(rKindNamespace, "%sdeclared runs artifact %q is not a runs sidecar of the artifact's parent", pre, id)
	}
	if id == d.RunsArtifact {
		ps.add(rKindNamespace, "%sdeclared runs artifact %q is also the captured runs sidecar", pre, id)
	}
	if refs[id] > 1 {
		ps.add(rKindNamespace, "%sdeclared runs artifact %q is referenced by more than one artifact", pre, id)
	}
}
