package evidence

// checkAcquisitionGaps checks the shape of Derivation.AcquisitionGaps. The
// overlap itself is a claim about the parent container, not recomputed here.
//
// The shape rule is symmetric: Bytes > 0 holds exactly when Ranges > 0 (an
// overlap has both a size and a count). A note with neither is valid only when
// Unknown (the overlap could not be decided: damaged error2, unrecorded runs);
// Unknown with both set is a lower bound. Anything else is rejected.
func checkAcquisitionGaps(ps *problemSet, r ManifestRecord, pre string) {
	d := r.Source.Derived
	if d == nil || d.AcquisitionGaps == nil {
		return
	}
	g := d.AcquisitionGaps
	switch {
	case r.Source.Kind == "carve" && d.Recovery != nil && d.Recovery.Scope == "artifact":
		ps.add(rKindShape, "%sacquisition gap note on a carve of scope artifact (its parent is not a container)", pre)
		return
	case r.Source.Kind != "extract" && r.Source.Kind != "unallocated" && r.Source.Kind != "carve":
		ps.add(rKindShape, "%sacquisition gap note on kind %q (only extract, unallocated and carve artifacts take one)", pre, r.Source.Kind)
		return
	case g.Bytes < 0 || g.Ranges < 0:
		ps.add(rKindShape, "%sacquisition gap note has a negative count", pre)
		return
	case !g.Unknown && g.Bytes == 0:
		ps.add(rKindShape, "%sacquisition gap note is empty (no overlapping bytes and not unknown)", pre)
	case g.Bytes > 0 && g.Ranges == 0:
		ps.add(rKindShape, "%sacquisition gap note has bytes but no ranges", pre)
	case g.Bytes == 0 && g.Ranges > 0:
		ps.add(rKindShape, "%sacquisition gap note has ranges but no bytes", pre)
	}
	if len(g.First) > MaxGapNoteRuns {
		ps.add(rKindShape, "%sacquisition gap note lists more than %d runs", pre, MaxGapNoteRuns)
	}
	for _, f := range g.First {
		if f.Offset < 0 || f.Length <= 0 {
			ps.add(rKindShape, "%sacquisition gap note has an invalid first run %d+%d", pre, f.Offset, f.Length)
			break
		}
	}
	if !r.Incomplete && !g.Unknown && g.Bytes > r.Size {
		ps.add(rKindShape, "%sacquisition gap note claims more gap bytes than the artifact holds (%d > %d)", pre, g.Bytes, r.Size)
	}
}
