package evidence

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"
)

// auditEntryFor builds the entry recordArtifact writes for r, as ReadAuditEntries would decode it.
func auditEntryFor(t *testing.T, seq int64, r ManifestRecord) AuditEntry {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id": r.ID, "path": r.Path, "size": r.Size, "sha256": r.SHA256, "md5": r.MD5,
		"source": r.Source, "incomplete": r.Incomplete, "error": r.Error,
	})
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var d map[string]any
	if err := dec.Decode(&d); err != nil {
		t.Fatal(err)
	}
	return AuditEntry{Seq: seq, Action: "artifact.create", Details: d}
}

func provRecord() ManifestRecord {
	return ManifestRecord{
		ID: "a1", Path: "artifacts/x/f.bin", Size: 4, SHA256: "s1", MD5: "m1",
		Source: Source{Kind: "file", DeviceID: "d", Derived: &Derivation{
			ParentID: "p", ParentSHA256: "ps", FSPath: "/etc/passwd",
			Runs:     []Run{{Offset: 4096, Length: 4}},
			Snapshot: &SnapshotRef{Name: "S", Xid: 5},
		}},
	}
}

func TestMaxDerivedDepthEqualsVerifyDepth(t *testing.T) {
	chain := func(links int) (map[string]ManifestRecord, string) {
		by := map[string]ManifestRecord{}
		id := func(i int) string { return "n" + strconv.Itoa(i) }
		for i := 0; i <= links; i++ {
			r := ManifestRecord{ID: id(i), Path: id(i)}
			if i < links {
				r.Source.Derived = &Derivation{ParentID: id(i + 1)}
			}
			by[r.ID] = r
		}
		return by, id(0)
	}
	if MaxDerivedDepth != 16 {
		t.Fatalf("MaxDerivedDepth = %d", MaxDerivedDepth)
	}
	by, root := chain(MaxDerivedDepth)
	if p := derivedChainProblem(by, root); p != "" {
		t.Errorf("%d links: %s", MaxDerivedDepth, p)
	}
	by, root = chain(MaxDerivedDepth + 1)
	if p := derivedChainProblem(by, root); p == "" {
		t.Errorf("%d links: no problem", MaxDerivedDepth+1)
	}
}

func TestAuditIndexBound(t *testing.T) {
	r := provRecord()
	x := NewAuditIndex([]AuditEntry{auditEntryFor(t, 7, r)})
	if got := x.Bind(r); got.State != AuditBound || got.Seq != 7 {
		t.Errorf("Bind = %+v", got)
	}
}

func TestAuditIndexDiffers(t *testing.T) {
	cases := map[string]struct {
		field string
		f     func(r *ManifestRecord)
	}{
		"path":       {"path", func(r *ManifestRecord) { r.Path = "artifacts/other" }},
		"size":       {"size", func(r *ManifestRecord) { r.Size = 5 }},
		"sha256":     {"sha256", func(r *ManifestRecord) { r.SHA256 = "x" }},
		"md5":        {"md5", func(r *ManifestRecord) { r.MD5 = "x" }},
		"incomplete": {"incomplete", func(r *ManifestRecord) { r.Incomplete = true }},
		"fs path":    {"source", func(r *ManifestRecord) { r.Source.Derived.FSPath = "/etc/shadow" }},
		"snapshot":   {"source", func(r *ManifestRecord) { r.Source.Derived.Snapshot = nil }},
		"runs":       {"source", func(r *ManifestRecord) { r.Source.Derived.Runs = []Run{{Offset: 8192, Length: 4}} }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			orig := provRecord()
			x := NewAuditIndex([]AuditEntry{auditEntryFor(t, 3, orig)})
			tampered := provRecord()
			tc.f(&tampered)
			got := x.Bind(tampered)
			if got.State != AuditDiffers || got.Detail != tc.field || got.Seq != 3 {
				t.Errorf("Bind = %+v, want differs on %q", got, tc.field)
			}
		})
	}
}

func TestAuditIndexMissingDuplicateUnreadable(t *testing.T) {
	r := provRecord()
	if got := NewAuditIndex(nil).Bind(r); got.State != AuditMissing {
		t.Errorf("no entry: %+v", got)
	}
	x := NewAuditIndex([]AuditEntry{auditEntryFor(t, 1, r), auditEntryFor(t, 2, r)})
	if got := x.Bind(r); got.State != AuditDuplicate {
		t.Errorf("two entries: %+v", got)
	}
	bad := auditEntryFor(t, 4, r)
	bad.Details["size"] = "not a number"
	if got := NewAuditIndex([]AuditEntry{bad}).Bind(r); got.State != AuditUnreadable || got.Seq != 4 {
		t.Errorf("size not a number: %+v", got)
	}
	noID := auditEntryFor(t, 5, r)
	delete(noID.Details, "id")
	if got := NewAuditIndex([]AuditEntry{noID}).Bind(r); got.State != AuditMissing {
		t.Errorf("entry without id: %+v", got)
	}
}

func TestAuditIndexIgnoresOtherActions(t *testing.T) {
	r := provRecord()
	e := auditEntryFor(t, 1, r)
	e.Action = "records.batch"
	if got := NewAuditIndex([]AuditEntry{e}).Bind(r); got.State != AuditMissing {
		t.Errorf("other action: %+v", got)
	}
}

func TestAuditIndexDecodesLazily(t *testing.T) {
	a, b := provRecord(), provRecord()
	b.ID = "b1"
	bad := auditEntryFor(t, 1, a)
	bad.Details["size"] = []any{"junk"}
	x := NewAuditIndex([]AuditEntry{bad, auditEntryFor(t, 2, b)})
	if got := x.Bind(b); got.State != AuditBound || got.Seq != 2 {
		t.Errorf("Bind(b) = %+v", got)
	}
	if got := x.Bind(a); got.State != AuditUnreadable {
		t.Errorf("Bind(a) = %+v", got)
	}
}

func TestAuditIndexMatchesVerifyOnRealCase(t *testing.T) {
	c, rec := derivedCase(t)
	entries, err := ReadAuditEntries(c.Dir + "/" + auditFile)
	if err != nil {
		t.Fatal(err)
	}
	man, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	x := NewAuditIndex(entries)
	for _, m := range man {
		if got := x.Bind(m); got.State != AuditBound {
			t.Errorf("%s: %+v", m.ID, got)
		}
	}
	rewriteManifest(t, c, func(r *ManifestRecord) {
		if r.ID == rec.ID {
			r.Source.Derived.FSPath = "/etc/shadow"
		}
	})
	rep := mustVerify(t, c)
	if rep.OK() || !containsSubstr(rep.Problems, "artifact "+rec.ID+" ("+rec.Path+"): source differs from audit") {
		t.Fatalf("verify = %+v", rep)
	}
	man, err = c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range man {
		if m.ID != rec.ID {
			continue
		}
		if got := x.Bind(m); got.State != AuditDiffers || got.Detail != "source" {
			t.Errorf("Bind after tamper = %+v", got)
		}
	}
}
