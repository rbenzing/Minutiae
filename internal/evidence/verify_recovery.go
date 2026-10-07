package evidence

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The recovery checks R1 to R5 of Verify (the R-series, separate from the record checks
// P1 to P14; R6 byte reproduction is a composed check, R7 runs with the records). They apply to
// every schema version, never abort, and list problems through a problemSet (50 per kind, then
// one count line). Problem texts are fixed by the plan ("Verify problem texts").

// Problem kinds of the R-series (the problemSet cap counts per kind).
const (
	rKindShape     = "recovered kind"
	rKindRecovery  = "recovery description"
	rKindNamespace = "recovered namespace"
	rKindRuns      = "recovered runs"
)

var ordinalPrefix = regexp.MustCompile(`^[0-9]{6,9}-`)

// recoveredKindNamespace returns the namespace directory of a recovered kind.
func recoveredKindNamespace(kind string) string {
	for _, ci := range classTable {
		if ci.Kind == kind {
			return ci.Namespace
		}
	}
	return ""
}

// unreproducedNotices adds, after the composed checks, a notice for every recovered kind this build
// does not reproduce byte for byte: slack, journal and report always; recover and carve until the
// composed reproduce check (R6) has run in this verify.
func unreproducedNotices(rep *VerifyReport) {
	for _, k := range []string{KindRecover, KindCarve, KindSlack, KindJournal, KindReport} {
		if (k == KindRecover || k == KindCarve) && rep.reproduceRan {
			continue
		}
		if n := rep.recoveredByKind[k]; n > 0 {
			rep.noticef("kind %q: %d artifact(s) are not reproduced byte for byte by this build (no rule for the kind yet)", k, n)
		}
	}
}

func (c *Case) verifyRecovered(rep *VerifyReport, recs []ManifestRecord) {
	ps := &problemSet{rep: rep, counts: map[string]int{}}
	defer ps.flush()

	uniq := make([]ManifestRecord, 0, len(recs))
	seen := make(map[string]bool, len(recs))
	byID := make(map[string]ManifestRecord, len(recs))
	for _, r := range recs {
		if seen[r.ID] {
			continue // a duplicate id is reported once by the manifest checks
		}
		seen[r.ID] = true
		byID[r.ID] = r
		uniq = append(uniq, r)
	}
	referenced := map[string]bool{} // runs sidecars some recovered artifact names
	for _, r := range uniq {
		if d := r.Source.Derived; d != nil && IsRecoveredKind(r.Source.Kind) && d.RunsArtifact != "" {
			referenced[d.RunsArtifact] = true
		}
	}

	byClass := map[string]int{}
	byKind := map[string]int{}
	var lowest *int
	for _, r := range uniq {
		pre := fmt.Sprintf("artifact %q (%q): ", r.ID, r.Path)
		kind := r.Source.Kind
		d := r.Source.Derived
		recovered := IsRecoveredKind(kind)

		// R1 (A4): kind and recovery description belong together
		switch {
		case recovered && d == nil:
			ps.add(rKindShape, "%shas no derivation (recovered artifacts need one)", pre)
		case recovered && d.Recovery == nil:
			ps.add(rKindShape, "%shas no recovery description", pre)
		case !recovered && d != nil && d.Recovery != nil:
			ps.add(rKindShape, "%scarries a recovery description (only kinds recover, carve, slack, journal and report may)", pre)
		}

		ns, inNS := RecoveredNamespace(r.Path)
		if recovered {
			rep.RecoveredArtifacts++
			byKind[kind]++
			if d != nil && d.Recovery != nil {
				cls := d.Recovery.Class
				if cls == "" {
					cls = "unknown"
				}
				byClass[cls]++
				if cf := d.Recovery.Confidence; cf != nil && (lowest == nil || *cf < *lowest) {
					v := *cf
					lowest = &v
				}
				// R2
				for _, p := range d.Recovery.Check(kind) {
					ps.add(rKindRecovery, "%s%s", pre, p)
				}
			}
			c.checkRecoveredPath(ps, r, pre, byID)
		} else if inNS {
			// A5: only a sidecar a recovered artifact names may live in a recovered namespace
			switch {
			case kind == "runs" && referenced[r.ID]:
			case kind == "runs":
				ps.add(rKindNamespace, "%sis not referenced by any recovered artifact (a runs sidecar in the %q namespace)", pre, ns)
			default:
				ps.add(rKindNamespace, "%sis in the recovered namespace %q but is of kind %q", pre, ns, kind)
			}
		}

		// R4 and R5: the runs of recovered files and carved bytes (R5 of every class bound to the unallocated scope)
		if recovered && d != nil {
			runs, err := c.DerivedRuns(r, byID)
			strict := kind == KindRecover || kind == KindCarve // the others may record no image runs (a report)
			if err != nil {
				ps.add(rKindRuns, "%sruns sidecar unreadable: %v", pre, err)
			} else if strict || len(runs) > 0 {
				for _, p := range CheckRecoveredRuns(r, runs) {
					ps.add(rKindRuns, "%s", p)
				}
				if parent, ok := byID[d.ParentID]; ok && c.isSingleRaw(parent) {
					for _, p := range runsBeyondParent(pre, runs, parent.Size) {
						ps.add(rKindRuns, "%s", p)
					}
				}
			}
		}
	}

	if rep.RecoveredArtifacts == 0 {
		return
	}
	rep.noticef("%s", recoveredNotice(rep.RecoveredArtifacts, byClass, lowest))
	rep.recoveredByKind = byKind
}

// checkRecoveredPath is R3 for one recovered artifact.
func (c *Case) checkRecoveredPath(ps *problemSet, r ManifestRecord, pre string, byID map[string]ManifestRecord) {
	kind := r.Source.Kind
	want := recoveredKindNamespace(kind)
	ns, inNS := RecoveredNamespace(r.Path)
	if !inNS || ns != want {
		ps.add(rKindNamespace, "%sis outside the namespace %q of kind %q", pre, want, kind)
	} else if kind == KindRecover {
		parts := strings.Split(r.Path, "/")
		if d := r.Source.Derived; d != nil {
			dir, err := SanitizeRelPath(fmt.Sprintf("p%d-%s", d.Partition, d.FSType))
			if err != nil || parts[4] != dir {
				ps.add(rKindNamespace, "%spath directory %q does not match derivation partition %d (%q)", pre, parts[4], d.Partition, d.FSType)
			}
		}
		if len(parts) < 6 || !ordinalPrefix.MatchString(parts[5]) {
			ps.add(rKindNamespace, "%sfile name does not start with an ordinal (six to nine digits and a dash)", pre)
		}
	}
	if d := r.Source.Derived; d != nil && d.RunsArtifact != "" {
		// a sidecar missing from the manifest is reported by the derived checks and the runs check
		if sc, ok := byID[d.RunsArtifact]; ok && (sc.Source.Kind != "runs" || path.Dir(sc.Path) != path.Dir(r.Path)) {
			ps.add(rKindNamespace, "%sruns artifact %q is not a runs sidecar in the same directory", pre, d.RunsArtifact)
		}
	}
}

var recoveredClassOrder = func() []string {
	var out []string
	for _, ci := range classTable {
		out = append(out, ci.Class)
	}
	return out
}()

func recoveredNotice(total int, byClass map[string]int, lowest *int) string {
	var parts []string
	for _, cls := range recoveredClassOrder {
		if n := byClass[cls]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", cls, n))
		}
	}
	var others []string
	for cls := range byClass {
		if !slices.Contains(recoveredClassOrder, cls) {
			others = append(others, cls)
		}
	}
	slices.Sort(others)
	for _, cls := range others {
		parts = append(parts, fmt.Sprintf("%q %d", cls, byClass[cls]))
	}
	s := fmt.Sprintf("%d recovered artifact(s) (%s)", total, strings.Join(parts, ", "))
	if lowest != nil {
		s += fmt.Sprintf("; lowest confidence %d", *lowest)
	}
	return s + "; recovered data is not live evidence"
}

// ewfSignature is the first 8 bytes of an E01 segment; its media is larger than the file.
const ewfSignature = "EVF\x09\x0d\x0a\xff\x00"

// isSingleRaw reports whether p is one raw image file whose size is the size of the media:
// a one-segment import that is not an EWF segment. R6 checks every other format.
func (c *Case) isSingleRaw(p ManifestRecord) bool {
	if p.Source.Kind != "import" || p.Source.Derived != nil || p.Source.Segments > 1 {
		return false
	}
	rel, err := SanitizeRelPath(p.Path)
	if err != nil {
		return false
	}
	f, err := os.Open(filepath.Join(c.Dir, rel)) //nolint:gosec // a manifest path made case-relative above
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, len(ewfSignature))
	n, _ := io.ReadFull(f, head)
	return string(head[:n]) != ewfSignature
}

// runsBeyondParent is the defence-in-depth part of R4: no run of an artifact copied from a
// single raw parent may end past the parent's size (overflow-checked).
func runsBeyondParent(pre string, runs []Run, parentSize int64) []string {
	var out []string
	for i, r := range runs {
		if r.Offset < 0 || r.Length < 0 {
			continue // reported as not valid by CheckRecoveredRuns (holes included)
		}
		if r.Offset > parentSize || r.Length > parentSize-r.Offset {
			out = append(out, fmt.Sprintf("%srun %d (offset %d, length %d) lies beyond the parent's %d bytes", pre, i, r.Offset, r.Length, parentSize))
		}
	}
	return out
}
