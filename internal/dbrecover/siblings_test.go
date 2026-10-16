package dbrecover

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func pulled(id, dev, acq, name, remote string) evidence.ManifestRecord {
	return evidence.ManifestRecord{
		ID: id, Path: "artifacts/" + dev + "/" + acq + "/" + name,
		Source: evidence.Source{Kind: "file", DeviceID: dev, RemotePath: remote},
	}
}

func fsArt(id, kind, parent string, part int, fsPath string, snap *evidence.SnapshotRef) evidence.ManifestRecord {
	return evidence.ManifestRecord{
		ID: id, Path: "artifacts/extract/" + id,
		Source: evidence.Source{Kind: kind, Derived: &evidence.Derivation{
			ParentID: parent, Partition: part, FSPath: fsPath, Snapshot: snap,
		}},
	}
}

func TestSiblingsByDeviceAndPath(t *testing.T) {
	db := pulled("A1", "d1", "acq1", "x.db", "/data/x.db")
	man := []evidence.ManifestRecord{
		db,
		pulled("A2", "d1", "acq1", "x.db-wal", "/data/x.db-wal"),
		pulled("A3", "d1", "acq1", "x.db-journal", "/data/x.db-journal"),
		pulled("A4", "d2", "acq1", "x.db-wal", "/data/x.db-wal"), // another device
		func() evidence.ManifestRecord { // same acquisition path, other device id
			r := pulled("A5", "d1", "acq1", "o.db-journal", "/data/x.db-journal")
			r.Source.DeviceID = "d2"
			return r
		}(),
	}
	r := FindSiblings(man, db)
	if r.WAL.Status != SiblingFound || r.WAL.ArtifactID != "A2" || r.Journal.Status != SiblingFound || r.Journal.ArtifactID != "A3" {
		t.Fatalf("%+v", r)
	}
	if r.WAL.SearchedBy == "" {
		t.Error("a found sibling does not say how it was found")
	}
}

func TestSiblingOtherAcquisitionIsNotPaired(t *testing.T) {
	a := pulled("A1", "d1", "acqA", "x.db", "/data/x.db")
	aw := pulled("A2", "d1", "acqA", "x.db-wal", "/data/x.db-wal")
	b := pulled("B1", "d1", "acqB", "x.db", "/data/x.db")
	r := FindSiblings([]evidence.ManifestRecord{a, aw, b}, b)
	if r.WAL.Status != SiblingOtherAcquisition || r.WAL.ArtifactID != "" {
		t.Fatalf("%+v", r.WAL)
	}
	if len(r.WAL.OtherAcquisition) != 1 || r.WAL.OtherAcquisition[0] != "A2" {
		t.Errorf("other acquisition %v", r.WAL.OtherAcquisition)
	}
	txt := r.WAL.Text("WAL")
	if !strings.Contains(txt, "none found in this acquisition") || !strings.Contains(txt, "A2") {
		t.Errorf("text %q", txt)
	}
	if r.Journal.Status != SiblingNone {
		t.Errorf("journal %+v", r.Journal)
	}
}

func TestSiblingsByParentPartitionAndFSPath(t *testing.T) {
	db := fsArt("E1", "extract", "IMG", 1, "/data/x.db", nil)
	man := []evidence.ManifestRecord{
		db,
		fsArt("E2", "extract", "IMG", 1, "/data/x.db-wal", nil),
		fsArt("E3", "extract", "IMG", 2, "/data/x.db-journal", nil), // another partition
		fsArt("E4", "extract", "OTHER", 1, "/data/x.db-journal", nil),
	}
	r := FindSiblings(man, db)
	if r.WAL.Status != SiblingFound || r.WAL.ArtifactID != "E2" {
		t.Errorf("wal %+v", r.WAL)
	}
	if r.Journal.Status != SiblingNone {
		t.Errorf("journal %+v: another partition or parent was paired", r.Journal)
	}
}

func TestSiblingSnapshotMustMatch(t *testing.T) {
	s1 := &evidence.SnapshotRef{Name: "snap1", Xid: 7}
	s1b := &evidence.SnapshotRef{Name: "snap1", Xid: 7}
	s2 := &evidence.SnapshotRef{Name: "snap1", Xid: 8}
	db := fsArt("E1", "extract", "IMG", 1, "/x.db", s1)
	man := []evidence.ManifestRecord{
		db,
		fsArt("E2", "extract", "IMG", 1, "/x.db-wal", nil),    // live tree
		fsArt("E3", "extract", "IMG", 1, "/x.db-journal", s2), // another snapshot
		fsArt("E4", "extract", "IMG", 1, "/x.db-wal", s1b),    // same snapshot
	}
	r := FindSiblings(man, db)
	if r.WAL.Status != SiblingFound || r.WAL.ArtifactID != "E4" {
		t.Errorf("wal %+v", r.WAL)
	}
	if r.Journal.Status != SiblingNone {
		t.Errorf("journal %+v", r.Journal)
	}
	live := fsArt("L1", "extract", "IMG", 1, "/x.db", nil)
	r = FindSiblings(append(man, live), live)
	if r.WAL.Status != SiblingFound || r.WAL.ArtifactID != "E2" {
		t.Errorf("a live db paired with %+v", r.WAL)
	}
}

func TestSiblingAmbiguityIsRefused(t *testing.T) {
	db := pulled("A1", "d1", "acq1", "x.db", "/data/x.db")
	man := []evidence.ManifestRecord{
		db,
		pulled("A3", "d1", "acq1", "copy2/x.db-wal", "/data/x.db-wal"),
		pulled("A2", "d1", "acq1", "x.db-wal", "/data/x.db-wal"),
	}
	r := FindSiblings(man, db)
	if r.WAL.Status != SiblingAmbiguous || r.WAL.ArtifactID != "" {
		t.Fatalf("%+v", r.WAL)
	}
	if len(r.WAL.Candidates) != 2 || r.WAL.Candidates[0] != "A3" || r.WAL.Candidates[1] != "A2" {
		t.Errorf("candidates %v, want manifest order", r.WAL.Candidates)
	}
	err := r.WAL.Err("wal")
	if !errors.Is(err, ErrAmbiguousSibling) || !strings.Contains(err.Error(), "A2") || !strings.Contains(err.Error(), "A3") {
		t.Errorf("err %v", err)
	}
	if (SiblingResult{Status: SiblingFound}).Err("wal") != nil {
		t.Error("a found sibling is an error")
	}
}

func TestSiblingRecoveredDatabaseFindsLiveWAL(t *testing.T) {
	db := fsArt("R1", "recover", "IMG", 1, "/data/x.db", nil)
	man := []evidence.ManifestRecord{db, fsArt("E2", "extract", "IMG", 1, "/data/x.db-wal", nil)}
	r := FindSiblings(man, db)
	if r.WAL.Status != SiblingFound || r.WAL.ArtifactID != "E2" {
		t.Errorf("%+v", r.WAL)
	}
}

func TestSiblingNoneIsReportedNotAbsent(t *testing.T) {
	db := pulled("A1", "d1", "acq1", "x.db", "/data/x.db")
	r := FindSiblings([]evidence.ManifestRecord{db}, db)
	for role, res := range map[string]SiblingResult{"WAL": r.WAL, "journal": r.Journal} {
		if res.Status != SiblingNone || res.SearchedBy == "" {
			t.Errorf("%s: %+v", role, res)
		}
		txt := res.Text(role)
		low := strings.ToLower(txt)
		if !strings.Contains(txt, "none found") || !strings.Contains(txt, "searched by") || strings.Contains(low, "no wal") || strings.Contains(low, "no journal") {
			t.Errorf("%s text %q", role, txt)
		}
	}
	if !strings.Contains(r.WAL.SearchedBy, `"/data/x.db-wal"`) {
		t.Errorf("searched by %q does not quote the path", r.WAL.SearchedBy)
	}
}

func TestSiblingBackupKindReportsNoneFoundNotNoWAL(t *testing.T) {
	db := evidence.ManifestRecord{
		ID: "B1", Path: "artifacts/udid/bk1/ab/abcdef0123",
		Source: evidence.Source{Kind: "backup", DeviceID: "udid", RemotePath: "ab/abcdef0123"},
	}
	r := FindSiblings([]evidence.ManifestRecord{db}, db)
	for _, res := range []SiblingResult{r.WAL, r.Journal} {
		if res.Status != SiblingNone || res.SearchedBy == "" {
			t.Errorf("%+v", res)
		}
		if strings.Contains(strings.ToLower(res.Text("WAL")), "no wal") {
			t.Errorf("text %q", res.Text("WAL"))
		}
	}
}

func TestSiblingExactNameOnly(t *testing.T) {
	db := pulled("A1", "d1", "acq1", "x.db", "/data/x.db")
	man := []evidence.ManifestRecord{
		db,
		pulled("A2", "d1", "acq1", "a", "/data/x.db-WAL"),
		pulled("A3", "d1", "acq1", "b", "/data/x.db-wal2"),
		pulled("A4", "d1", "acq1", "c", "/data/y.db-wal"),
		pulled("A5", "d1", "acq1", "d", "/data/x.db-wal/"),
		pulled("A6", "d1", "acq1", "e", "data/x.db-wal"),
		pulled("A7", "d1", "acq1", "f", "/data/x.db-shm"),
	}
	r := FindSiblings(man, db)
	if r.WAL.Status != SiblingNone || r.Journal.Status != SiblingNone {
		t.Errorf("%+v", r)
	}
}

func TestSiblingCarveHasNoIdentity(t *testing.T) {
	db := evidence.ManifestRecord{ID: "C1", Path: "carved/raw/c1.db", Source: evidence.Source{
		Kind:    evidence.KindCarve,
		Derived: &evidence.Derivation{ParentID: "IMG", FSPath: "/x.db"},
	}}
	man := []evidence.ManifestRecord{db, fsArt("E2", "extract", "IMG", 0, "/x.db-wal", nil)}
	r := FindSiblings(man, db)
	if r.WAL.Status != SiblingNoIdentity || r.Journal.Status != SiblingNoIdentity {
		t.Errorf("%+v", r)
	}
	if txt := r.WAL.Text("WAL"); strings.Contains(strings.ToLower(txt), "no wal") || txt == "" {
		t.Errorf("text %q", txt)
	}
}

func TestSiblingNoIdentityWithoutPaths(t *testing.T) {
	noRemote := pulled("A1", "d1", "acq1", "x.db", "")
	shortPath := evidence.ManifestRecord{ID: "A2", Path: "weird", Source: evidence.Source{Kind: "file", DeviceID: "d1", RemotePath: "/x.db"}}
	noFS := fsArt("E1", "extract", "IMG", 1, "", nil)
	noDerived := evidence.ManifestRecord{ID: "E2", Source: evidence.Source{Kind: "recover"}}
	for _, db := range []evidence.ManifestRecord{noRemote, shortPath, noFS, noDerived} {
		r := FindSiblings([]evidence.ManifestRecord{db}, db)
		if r.WAL.Status != SiblingNoIdentity || r.Journal.Status != SiblingNoIdentity {
			t.Errorf("%s: %+v", db.ID, r)
		}
	}
}

func TestSiblingOrderIsDeterministic(t *testing.T) {
	db := pulled("A1", "d1", "acq1", "x.db", "/x.db")
	man := []evidence.ManifestRecord{db}
	for _, id := range []string{"W9", "W3", "W5", "W1"} {
		man = append(man, pulled(id, "d1", "acq1", id, "/x.db-wal"))
	}
	first := FindSiblings(man, db)
	for i := 0; i < 20; i++ {
		got := FindSiblings(man, db)
		if strings.Join(got.WAL.Candidates, ",") != "W9,W3,W5,W1" || got.WAL.SearchedBy != first.WAL.SearchedBy {
			t.Fatalf("run %d: %v", i, got.WAL)
		}
	}
}

func TestSiblingHostilePaths(t *testing.T) {
	hostile := "/data/x.db\x00\x1b[31m\n"
	db := pulled("A1", "d1", "acq1", "x.db", hostile)
	man := []evidence.ManifestRecord{
		db,
		pulled("A2", "d1", "acq1", "w", hostile+"-wal"),
		pulled("A3", "d1", "acq1", "w2", strings.Repeat("/..", 100000)),
		{ID: "A4", Path: "artifacts/d1", Source: evidence.Source{Kind: "file", DeviceID: "d1", RemotePath: hostile + "-wal"}},
		{ID: "A5", Path: "../../etc/passwd", Source: evidence.Source{Kind: "file", DeviceID: "d1", RemotePath: hostile + "-wal"}},
	}
	r := FindSiblings(man, db)
	if r.WAL.Status != SiblingFound || r.WAL.ArtifactID != "A2" {
		t.Errorf("%+v", r.WAL)
	}
	for _, s := range []string{r.WAL.SearchedBy, r.Journal.SearchedBy} {
		if strings.ContainsAny(s, "\x00\x1b\n") {
			t.Errorf("searched by %q holds raw control characters (it must be %%q-quoted)", s)
		}
	}
	long := pulled("L1", "d1", "acq1", "l.db", strings.Repeat("a", 1<<20))
	if got := FindSiblings([]evidence.ManifestRecord{long}, long); got.WAL.Status != SiblingNone {
		t.Errorf("long path: %+v", got.WAL)
	}
}

func TestFindSiblingsRecoversPanic(t *testing.T) {
	restore := SetSiblingScanForTest(func([]evidence.ManifestRecord, evidence.ManifestRecord) SiblingReport {
		panic("poisoned manifest")
	})
	defer restore()
	db := pulled("A1", "d1", "acq1", "x.db", "/x.db")
	r := FindSiblings([]evidence.ManifestRecord{db}, db)
	for _, res := range []SiblingResult{r.WAL, r.Journal} {
		if res.Status == SiblingFound || res.Status == SiblingNone || res.SearchedBy == "" {
			t.Errorf("a panic became %+v", res)
		}
	}
}
