package records

import (
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// fuzzManifest decodes raw into at most 64 manifest records whose ids, parents, recorded parent
// hashes and segment lists are all driven by the bytes, hostile shapes included (duplicate ids,
// self parents, cycles, missing parents, wrong hashes). It also returns which records have an
// audit entry.
func fuzzManifest(raw []byte) (man []evidence.ManifestRecord, audited []evidence.ManifestRecord) {
	next := func() int {
		if len(raw) == 0 {
			return 0
		}
		b := raw[0]
		raw = raw[1:]
		return int(b)
	}
	n := next()%64 + 1
	id := func(v int) string { return "a" + strconv.Itoa(v%(n+2)) } // a few ids point outside the manifest
	for i := 0; i < n && len(raw) > 0; i++ {
		r := evidence.ManifestRecord{ID: id(next()%3 + i), Path: "p" + strconv.Itoa(i), Size: 10, SHA256: "s" + strconv.Itoa(next()%4), MD5: "m"}
		r.Source = evidence.Source{Kind: "file"}
		if kind := next() % 4; kind != 0 {
			d := &evidence.Derivation{ParentID: id(next()), ParentSHA256: "s" + strconv.Itoa(next()%4)}
			for s, ns := 0, next()%4; s < ns; s++ {
				d.ParentSegments = append(d.ParentSegments, evidence.SegmentRef{ID: id(next()), SHA256: "s" + strconv.Itoa(next()%4)})
			}
			r.Source.Kind, r.Source.Derived = "extract", d
		}
		man = append(man, r)
		if next()%4 != 0 {
			audited = append(audited, r)
		}
	}
	return man, audited
}

func FuzzResolveChain(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{3, 0, 0, 1, 0, 1, 1, 0, 0, 0, 1, 0})
	f.Add([]byte{5, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1})                    // self and tight cycles
	f.Add([]byte{63, 7, 2, 9, 4, 3, 0, 1, 2, 3, 1, 2, 3, 1, 2, 3, 1, 3, 3, 3, 2, 1, 1, 1, 1, 0, 0, 0}) // segments
	f.Fuzz(func(t *testing.T, raw []byte) {
		man, audited := fuzzManifest(raw)
		ix := evidence.NewAuditIndex(auditOf(t, audited...))
		starts := []string{"a0", "a1", "a63", "nope"}
		for _, m := range man {
			starts = append(starts, m.ID)
		}
		for _, start := range starts {
			p := resolveChain(man, ix, start)
			if len(p.Chain) > evidence.MaxDerivedDepth+1 {
				t.Fatalf("chain of %d hops", len(p.Chain))
			}
			seen := map[string]bool{}
			for _, h := range p.Chain {
				if seen[h.Artifact.ID] {
					t.Fatalf("artifact %q appears twice in the chain", h.Artifact.ID)
				}
				seen[h.Artifact.ID] = true
			}
			for _, pr := range p.Problems {
				if pr.Hop < -1 || pr.Hop > len(p.Chain)-1 {
					t.Fatalf("problem %+v has hop outside the chain of %d", pr, len(p.Chain))
				}
			}
			if p.ReachedRoot && (len(p.Chain) == 0 || p.Chain[len(p.Chain)-1].Artifact.Source.Derived != nil) {
				t.Fatalf("ReachedRoot with the last hop %+v", p.Chain)
			}
			if !p.ReachedRoot && p.OK() {
				t.Fatalf("a chain that did not reach a root must carry a problem: %+v", p)
			}
		}
	})
}
