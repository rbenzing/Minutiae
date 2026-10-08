package records

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// pnode is a manifest record "id" derived from parent ("" = no derivation).
func pnode(id, parent string) evidence.ManifestRecord {
	r := evidence.ManifestRecord{
		ID: id, Path: "artifacts/" + id, Size: 10, SHA256: "sha-" + id, MD5: "md5-" + id,
		Source: evidence.Source{Kind: "file"},
	}
	if parent != "" {
		r.Source.Kind = "extract"
		r.Source.Derived = &evidence.Derivation{ParentID: parent, ParentSHA256: "sha-" + parent}
	}
	return r
}

// auditOf builds the artifact.create entries Case.NewArtifact would append.
func auditOf(t testing.TB, recs ...evidence.ManifestRecord) []evidence.AuditEntry {
	t.Helper()
	var out []evidence.AuditEntry
	for i, r := range recs {
		b, err := json.Marshal(r.Source)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var src any
		if err := dec.Decode(&src); err != nil {
			t.Fatal(err)
		}
		out = append(out, evidence.AuditEntry{Seq: int64(i + 1), Action: "artifact.create", Details: map[string]any{
			"id": r.ID, "path": r.Path, "size": r.Size, "sha256": r.SHA256, "md5": r.MD5,
			"source": src, "incomplete": r.Incomplete, "error": r.Error,
		}})
	}
	return out
}

func resolve(t testing.TB, man []evidence.ManifestRecord, id string) Provenance {
	t.Helper()
	return resolveChain(man, evidence.NewAuditIndex(auditOf(t, man...)), id)
}

func chainIDs(p Provenance) []string {
	var ids []string
	for _, h := range p.Chain {
		ids = append(ids, h.Artifact.ID)
	}
	return ids
}

func linksOf(p Provenance) []LinkState {
	var l []LinkState
	for _, h := range p.Chain {
		l = append(l, h.Link)
	}
	return l
}

func kindsOf(p Provenance) map[ProblemKind]int {
	m := map[ProblemKind]int{}
	for _, pr := range p.Problems {
		m[pr.Kind]++
	}
	return m
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func eqLinks(a, b []LinkState) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChainLiveFileOnImage(t *testing.T) {
	img := pnode("img", "")
	img.Source.Kind = "import"
	img.Source.Segments = 3
	part := pnode("fs", "img")
	file := pnode("file", "fs")
	p := resolve(t, []evidence.ManifestRecord{img, part, file}, "file")
	if !p.OK() || !p.ReachedRoot {
		t.Fatalf("%+v", p)
	}
	if !eqStrings(chainIDs(p), []string{"file", "fs", "img"}) || !eqLinks(linksOf(p), []LinkState{LinkOK, LinkOK, LinkRoot}) {
		t.Fatalf("chain %v links %v", chainIDs(p), linksOf(p))
	}
	for i, h := range p.Chain {
		if h.Audit.State != evidence.AuditBound {
			t.Fatalf("hop %d audit %+v", i, h.Audit)
		}
	}
	if p.Offset.Coordinates != "media" || p.Offset.Segments != 3 || p.Offset.State != "" || p.Recovery != nil {
		t.Fatalf("offset %+v recovery %v", p.Offset, p.Recovery)
	}
}

func linear(n int) []evidence.ManifestRecord {
	// a0 derived from a1 ... a(n-1) is the root: n-1 links from a0.
	var man []evidence.ManifestRecord
	for i := 0; i < n; i++ {
		parent := ""
		if i < n-1 {
			parent = fmt.Sprintf("a%d", i+1)
		}
		man = append(man, pnode(fmt.Sprintf("a%d", i), parent))
	}
	return man
}

func TestChainDeepButLegal(t *testing.T) {
	p := resolve(t, linear(17), "a0") // 16 links
	if !p.OK() || !p.ReachedRoot || len(p.Chain) != 17 {
		t.Fatalf("ok %v root %v hops %d problems %+v", p.OK(), p.ReachedRoot, len(p.Chain), p.Problems)
	}
}

func TestChainTooDeep(t *testing.T) {
	p := resolve(t, linear(18), "a0") // 17 links
	if p.ReachedRoot || len(p.Chain) != 17 {
		t.Fatalf("root %v hops %d", p.ReachedRoot, len(p.Chain))
	}
	if last := p.Chain[16]; last.Link != LinkTooDeep || last.Artifact.ID != "a16" {
		t.Fatalf("last hop %+v", last)
	}
	if len(p.Problems) != 1 || p.Problems[0].Kind != ProblemTooDeep || p.Problems[0].Hop != 16 {
		t.Fatalf("%+v", p.Problems)
	}
}

func TestChainHostileShapes(t *testing.T) {
	root := pnode("root", "")
	twice := []evidence.ManifestRecord{root, root}
	diff := pnode("child", "root")
	diff.Source.Derived.ParentSHA256 = "sha-other"
	empty := pnode("child", "x")
	empty.Source.Derived.ParentID = ""
	deepMissing := linear(17) // a16 is the root; make a16 derived from a missing parent: link 17
	deepMissing[16].Source.Derived = &evidence.Derivation{ParentID: "gone", ParentSHA256: "x"}

	tests := []struct {
		name  string
		man   []evidence.ManifestRecord
		start string
		ids   []string
		links []LinkState
		root  bool
		kinds map[ProblemKind]int
	}{
		{
			"self parent",
			[]evidence.ManifestRecord{pnode("a", "a")},
			"a",
			[]string{"a"},
			[]LinkState{LinkCycle},
			false,
			map[ProblemKind]int{ProblemCycle: 1},
		},
		{
			"two cycle",
			[]evidence.ManifestRecord{pnode("a", "b"), pnode("b", "a")},
			"a",
			[]string{"a", "b"},
			[]LinkState{LinkOK, LinkCycle},
			false,
			map[ProblemKind]int{ProblemCycle: 1},
		},
		{
			"three cycle through start",
			[]evidence.ManifestRecord{pnode("a", "b"), pnode("b", "c"), pnode("c", "a")},
			"a",
			[]string{"a", "b", "c"},
			[]LinkState{LinkOK, LinkOK, LinkCycle},
			false,
			map[ProblemKind]int{ProblemCycle: 1},
		},
		{
			"parent missing",
			[]evidence.ManifestRecord{pnode("a", "gone")},
			"a",
			[]string{"a"},
			[]LinkState{LinkParentMissing},
			false,
			map[ProblemKind]int{ProblemParentMissing: 1},
		},
		{
			"parent id twice", append([]evidence.ManifestRecord{pnode("a", "root")}, twice...), "a",
			[]string{"a"},
			[]LinkState{LinkParentAmbiguous},
			false,
			map[ProblemKind]int{ProblemParentAmbiguous: 1},
		},
		{
			"parent hash differs continues",
			[]evidence.ManifestRecord{root, diff},
			"child",
			[]string{"child", "root"},
			[]LinkState{LinkOK, LinkHashDiffers},
			true,
			map[ProblemKind]int{ProblemParentHash: 1},
		},
		{
			"empty parent id",
			[]evidence.ManifestRecord{empty},
			"child",
			[]string{"child"},
			[]LinkState{LinkParentMissing},
			false,
			map[ProblemKind]int{ProblemParentMissing: 1},
		},
		{
			"parent without derivation is the root",
			[]evidence.ManifestRecord{root, pnode("a", "root")},
			"a",
			[]string{"a", "root"},
			[]LinkState{LinkOK, LinkRoot},
			true, nil,
		},
		{
			"missing parent at link 17", deepMissing, "a0",
			nil, nil, false,
			map[ProblemKind]int{ProblemParentMissing: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := resolve(t, tc.man, tc.start)
			if tc.ids != nil && (!eqStrings(chainIDs(p), tc.ids) || !eqLinks(linksOf(p), tc.links)) {
				t.Fatalf("chain %v links %v", chainIDs(p), linksOf(p))
			}
			if p.ReachedRoot != tc.root {
				t.Fatalf("ReachedRoot %v", p.ReachedRoot)
			}
			got := kindsOf(p)
			if len(got) != len(tc.kinds) {
				t.Fatalf("problems %+v want %v", p.Problems, tc.kinds)
			}
			for k, n := range tc.kinds {
				if got[k] != n {
					t.Fatalf("problems %+v want %v", p.Problems, tc.kinds)
				}
			}
			if p.OK() != (len(tc.kinds) == 0) {
				t.Fatalf("OK %v", p.OK())
			}
		})
	}
	t.Run("missing parent at link 17 is missing here", func(t *testing.T) {
		p := resolve(t, deepMissing, "a0")
		if len(p.Chain) != 17 || p.Chain[16].Link != LinkParentMissing {
			t.Fatalf("hops %d", len(p.Chain))
		}
	})
	t.Run("start not in manifest", func(t *testing.T) {
		p := resolve(t, []evidence.ManifestRecord{root}, "nope")
		if len(p.Chain) != 0 || p.ReachedRoot || len(p.Problems) != 1 || p.Problems[0].Hop != -1 || p.Problems[0].Kind != ProblemParentMissing {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("start id twice", func(t *testing.T) {
		p := resolve(t, twice, "root")
		if len(p.Chain) != 0 || len(p.Problems) != 1 || p.Problems[0].Kind != ProblemParentAmbiguous || p.Problems[0].Hop != -1 {
			t.Fatalf("%+v", p)
		}
	})
}

func segs(n int, bad func(i int) string) ([]evidence.SegmentRef, []evidence.ManifestRecord) {
	var refs []evidence.SegmentRef
	var recs []evidence.ManifestRecord
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("seg%d", i)
		ref := evidence.SegmentRef{ID: id, SHA256: "sha-" + id}
		kind := ""
		if bad != nil {
			kind = bad(i)
		}
		switch kind {
		case "missing":
		case "hash":
			recs = append(recs, pnode(id, ""))
			ref.SHA256 = "wrong"
		case "twice":
			recs = append(recs, pnode(id, ""), pnode(id, ""))
		default:
			recs = append(recs, pnode(id, ""))
		}
		refs = append(refs, ref)
	}
	return refs, recs
}

func withSegments(refs []evidence.SegmentRef, recs []evidence.ManifestRecord) []evidence.ManifestRecord {
	part := pnode("part", "seg0")
	part.Source.Derived.ParentSegments = refs
	return append([]evidence.ManifestRecord{part}, recs...)
}

func TestChainSegmentsChecked(t *testing.T) {
	cases := []struct {
		name  string
		bad   func(int) string
		state string
	}{
		{"good", nil, ""},
		{"one missing", func(i int) string {
			if i == 1 {
				return "missing"
			}
			return ""
		}, "missing"},
		{"one hash differs", func(i int) string {
			if i == 2 {
				return "hash"
			}
			return ""
		}, "hash-differs"},
		{"one id twice", func(i int) string {
			if i == 1 {
				return "twice"
			}
			return ""
		}, "ambiguous"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refs, recs := segs(3, tc.bad)
			// seg0 is the parent id of the part; keep it present and unique.
			p := resolve(t, withSegments(refs, recs), "part")
			h := p.Chain[0]
			if h.SegmentsTotal != 3 || len(h.Segments) != 3 {
				t.Fatalf("total %d shown %d", h.SegmentsTotal, len(h.Segments))
			}
			if tc.state == "" {
				if !p.OK() || h.SegmentsBad != 0 {
					t.Fatalf("%+v", p.Problems)
				}
				return
			}
			bad := 0
			for _, s := range h.Segments {
				if s.State == tc.state {
					bad++
				}
			}
			if bad != 1 || h.SegmentsBad != 1 || len(p.Problems) != 1 || p.Problems[0].Kind != ProblemSegment || p.Problems[0].Hop != 0 {
				t.Fatalf("bad %d/%d problems %+v", bad, h.SegmentsBad, p.Problems)
			}
		})
	}
	t.Run("100000 segments 50000 missing", func(t *testing.T) {
		const n = 100000
		refs, recs := segs(n, func(i int) string {
			if i%2 == 1 {
				return "missing"
			}
			return ""
		})
		man := withSegments(refs, recs)
		start := time.Now()
		p := resolve(t, man, "part")
		if d := time.Since(start); d > 20*time.Second {
			t.Fatalf("took %v", d)
		}
		h := p.Chain[0]
		if h.SegmentsTotal != n || h.SegmentsBad != n/2 || len(h.Segments) != MaxShownSegments {
			t.Fatalf("total %d bad %d shown %d", h.SegmentsTotal, h.SegmentsBad, len(h.Segments))
		}
		if len(p.Problems) != MaxProblemsPerKind || p.ProblemsSuppressed != n/2-MaxProblemsPerKind {
			t.Fatalf("listed %d suppressed %d", len(p.Problems), p.ProblemsSuppressed)
		}
		if p.OK() {
			t.Fatal("OK with suppressed problems")
		}
	})
}

func TestChainAuditStatesBecomeProblems(t *testing.T) {
	root := pnode("root", "")
	a := pnode("a", "root")
	b := pnode("b", "root")
	entries := auditOf(t, root, a)
	entries = append(entries, auditOf(t, a)[0]) // a: duplicate
	// b: differs (size changed after the audit entry was written)
	bEntry := auditOf(t, b)[0]
	b.Size = 99
	// c: unreadable details
	c := pnode("c", "root")
	entries = append(entries, bEntry, evidence.AuditEntry{Seq: 9, Action: "artifact.create", Details: map[string]any{"id": "c", "size": "not a number"}})
	// d: no entry at all
	d := pnode("d", "root")
	man := []evidence.ManifestRecord{root, a, b, c, d}
	ix := evidence.NewAuditIndex(entries)
	want := map[string]ProblemKind{"a": ProblemAuditDuplicate, "b": ProblemAuditDiffers, "c": ProblemAuditUnreadable, "d": ProblemAuditMissing}
	for id, kind := range want {
		p := resolveChain(man, ix, id)
		got := kindsOf(p)
		if got[kind] != 1 || len(p.Problems) != 1 || p.Problems[0].Hop != 0 || p.OK() {
			t.Fatalf("%s: %+v", id, p.Problems)
		}
		if p.Chain[0].Audit.State == evidence.AuditBound {
			t.Fatalf("%s bound", id)
		}
	}
	if p := resolveChain(man, ix, "root"); !p.OK() {
		t.Fatalf("%+v", p.Problems)
	}
}

func TestChainProblemCapPerKind(t *testing.T) {
	refs, recs := segs(60, func(int) string { return "missing" })
	part := pnode("part", "root")
	part.Source.Derived.ParentSegments = refs
	_ = recs
	man := []evidence.ManifestRecord{part, pnode("root", "")}
	// Add an audit problem of another kind: it has its own cap.
	ix := evidence.NewAuditIndex(auditOf(t, man[1]))
	p := resolveChain(man, ix, "part")
	k := kindsOf(p)
	if k[ProblemSegment] != 50 || k[ProblemAuditMissing] != 1 || p.ProblemsSuppressed != 10 || p.Chain[0].SegmentsBad != 60 {
		t.Fatalf("kinds %v suppressed %d", k, p.ProblemsSuppressed)
	}
}

func TestChainFieldsPassThrough(t *testing.T) {
	root := pnode("root", "")
	f := pnode("f", "root")
	d := f.Source.Derived
	d.Snapshot = &evidence.SnapshotRef{Name: "snap1", Xid: 261}
	d.FSPath, d.FSID, d.Partition, d.PartitionOffset = "/a/b", "cnid:20", 2, 1048576
	d.Encrypted, d.ParentIncomplete = true, true
	d.Recovery = &evidence.Recovery{Class: "c", Method: "m"}
	p := resolve(t, []evidence.ManifestRecord{root, f}, "f")
	got := p.Chain[0].Artifact.Source.Derived
	if got.Snapshot == nil || got.Snapshot.Name != "snap1" || got.Snapshot.Xid != 261 || got.FSPath != "/a/b" || got.FSID != "cnid:20" ||
		got.Partition != 2 || got.PartitionOffset != 1048576 || !got.Encrypted || !got.ParentIncomplete ||
		got.Recovery == nil || got.Recovery.Method != "m" || got.Recovery.Class != "c" {
		t.Fatalf("%+v", got)
	}
}

func TestProvenanceOKZeroValue(t *testing.T) {
	if !(Provenance{}).OK() {
		t.Fatal("zero value is not OK")
	}
	if (Provenance{Problems: []Problem{{Kind: ProblemCycle}}}).OK() || (Provenance{ProblemsSuppressed: 1}).OK() {
		t.Fatal("problems ignored")
	}
}
