package artparse_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// countingSource wraps a case and counts the reads of its manifest and audit log.
type countingSource struct {
	c                 *evidence.Case
	manifests, audits int
}

func (s *countingSource) Manifest() ([]evidence.ManifestRecord, error) {
	s.manifests++
	return s.c.Manifest()
}

func (s *countingSource) ReadAudit() ([]evidence.AuditEntry, error) {
	s.audits++
	return s.c.ReadAudit()
}

func TestSnapshotIsReadOnceAndDiscoverNeverReadsTheManifest(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", "/data/data/com.a/databases/app.db", "x")
	src := &countingSource{c: c}
	opt := artparse.Options{Limits: parse.DefaultLimits()}
	artparse.WithCaseSource(&opt, src)
	h, err := artparse.New(c, []artparse.Registered{register(t, "p", "1.0.0")}, opt)
	if err != nil {
		t.Fatal(err)
	}
	if src.manifests != 0 || src.audits != 0 {
		t.Fatalf("New read the case: manifest %d, audit %d", src.manifests, src.audits)
	}
	snap, err := h.TakeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if src.manifests != 1 || src.audits != 1 {
		t.Errorf("TakeSnapshot read manifest %d times and audit %d times, want 1 and 1", src.manifests, src.audits)
	}
	for i := 0; i < 2; i++ {
		jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
		if err != nil || len(jobs) != 1 {
			t.Fatalf("Discover: %d jobs, %v", len(jobs), err)
		}
	}
	if src.manifests != 1 || src.audits != 1 {
		t.Errorf("after Discover: manifest %d, audit %d, want 1 and 1", src.manifests, src.audits)
	}
}

func TestSnapshotRecordAndMatches(t *testing.T) {
	c := newCase(t)
	d := &evidence.Derivation{ParentID: "p", FSType: "ext4", FSPath: "/a", Snapshot: &evidence.SnapshotRef{Name: "s", Xid: 5}}
	rec := put(t, c, "D1", "A1", "x", evidence.Source{Kind: "extract", RemotePath: "/a", Derived: d}, "data")
	h := newHost(t, c)
	snap := snapshotOf(t, h)
	got, ok := snap.Record(rec.ID)
	if !ok || got.ID != rec.ID {
		t.Fatalf("Record(%s) = %+v, %v", rec.ID, got, ok)
	}
	if _, ok := snap.Record("nope"); ok {
		t.Error("Record found an unknown id")
	}
	if !snap.Matches(got) {
		t.Fatal("the snapshot's own record does not match")
	}
	mutations := map[string]func(m *evidence.ManifestRecord){
		"id":              func(m *evidence.ManifestRecord) { m.ID = "other" },
		"path":            func(m *evidence.ManifestRecord) { m.Path += "x" },
		"size":            func(m *evidence.ManifestRecord) { m.Size++ },
		"sha256":          func(m *evidence.ManifestRecord) { m.SHA256 = "00" + m.SHA256[2:] },
		"incomplete":      func(m *evidence.ManifestRecord) { m.Incomplete = !m.Incomplete },
		"source kind":     func(m *evidence.ManifestRecord) { m.Source.Kind = "file" },
		"source device":   func(m *evidence.ManifestRecord) { m.Source.DeviceID = "D2" },
		"source remote":   func(m *evidence.ManifestRecord) { m.Source.RemotePath = "/b" },
		"derived nil":     func(m *evidence.ManifestRecord) { m.Source.Derived = nil },
		"derived fs path": func(m *evidence.ManifestRecord) { cp := *m.Source.Derived; cp.FSPath = "/b"; m.Source.Derived = &cp },
		"snapshot xid": func(m *evidence.ManifestRecord) {
			cp := *m.Source.Derived
			cp.Snapshot = &evidence.SnapshotRef{Name: "s", Xid: 6}
			m.Source.Derived = &cp
		},
		"snapshot removed": func(m *evidence.ManifestRecord) { cp := *m.Source.Derived; cp.Snapshot = nil; m.Source.Derived = &cp },
	}
	for name, mut := range mutations {
		m := got
		mut(&m)
		if snap.Matches(m) {
			t.Errorf("a record with a different %s matches", name)
		}
	}
	// a record that equals the snapshot's in value but is a separate copy matches
	cp := got
	if d := *got.Source.Derived; true {
		cp.Source.Derived = &d
	}
	if !snap.Matches(cp) {
		t.Error("an equal copy does not match")
	}
}

func TestDiscoveryDuplicateManifestIDIsIntegrityError(t *testing.T) {
	for name, plant := range map[string]func(rec evidence.ManifestRecord) evidence.ManifestRecord{
		"id": func(rec evidence.ManifestRecord) evidence.ManifestRecord { rec.Path += ".2"; return rec },
		"path": func(rec evidence.ManifestRecord) evidence.ManifestRecord {
			rec.ID = "ffffffffffffffffffffffffffffffff"
			return rec
		},
	} {
		c := newCase(t)
		rec := putFile(t, c, "D1", "A1", "/data/data/com.a/databases/app.db", "x")
		recordstest.AppendManifestLine(t, c.Dir, plant(rec))
		h := newHost(t, c, register(t, "p", "1.0.0"))
		if _, err := h.TakeSnapshot(); !errors.Is(err, evidence.ErrIntegrity) {
			t.Errorf("duplicate %s: TakeSnapshot = %v, want evidence.ErrIntegrity", name, err)
		}
	}
}
