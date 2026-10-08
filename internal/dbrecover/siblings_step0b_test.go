package dbrecover

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func TestFindSiblingsPanicIsStatusErrorNeverNoIdentity(t *testing.T) {
	restore := SetSiblingScanForTest(func([]evidence.ManifestRecord, evidence.ManifestRecord) SiblingReport {
		panic("boom\x1b[31m\nsecond line")
	})
	defer restore()
	db := pulled("A1", "d1", "acq1", "x.db", "/x.db")
	r := FindSiblings([]evidence.ManifestRecord{db}, db)
	for role, res := range map[string]SiblingResult{"wal": r.WAL, "journal": r.Journal} {
		if res.Status != SiblingError {
			t.Fatalf("%s: status %q, want %q", role, res.Status, SiblingError)
		}
		if strings.ContainsAny(res.SearchedBy, "\x1b\n") || !strings.Contains(res.SearchedBy, `\x1b`) {
			t.Errorf("%s: text %q is not %%q-quoted", role, res.SearchedBy)
		}
		if txt := res.Text(role); strings.Contains(txt, "no path identity") || !strings.Contains(txt, "failed") {
			t.Errorf("%s: text %q", role, txt)
		}
		if err := res.Err(role); !errors.Is(err, ErrSiblingSearch) {
			t.Errorf("%s: Err %v", role, err)
		}
	}
}

func TestSiblingDuplicateDatabaseIDIsAnErrorStatus(t *testing.T) {
	db := pulled("A1", "d1", "acq1", "x.db", "/x.db")
	dup := pulled("A1", "d1", "acq1", "copy.db", "/copy.db")
	man := []evidence.ManifestRecord{db, pulled("A2", "d1", "acq1", "x.db-wal", "/x.db-wal"), dup}
	r := FindSiblings(man, db)
	for role, res := range map[string]SiblingResult{"wal": r.WAL, "journal": r.Journal} {
		if res.Status != SiblingError || !strings.Contains(res.SearchedBy, "A1") || !strings.Contains(strings.ToLower(res.SearchedBy), "duplicate") {
			t.Errorf("%s: %+v", role, res)
		}
		if !errors.Is(res.Err(role), ErrSiblingSearch) {
			t.Errorf("%s: Err %v", role, res.Err(role))
		}
	}
	one := FindSiblings(man[:2], db)
	if one.WAL.Status != SiblingFound {
		t.Errorf("a single database record: %+v", one.WAL)
	}
}

func TestSiblingExtractExactNameOnly(t *testing.T) {
	for _, kind := range []string{"extract", "recover"} {
		db := fsArt("E1", kind, "IMG", 1, "/data/x.db", nil)
		for _, bad := range []string{
			"/data/x.db-wal ", " /data/x.db-wal", "/data/x.db-WAL", "/data/X.db-wal",
			"/data/x.db-wal2", "/data/x.db-wal/", "data/x.db-wal", "/data/x.db-wal​",
			"/data//x.db-wal", "/data/./x.db-wal", "/data/x.db‐wal", "/data/x.db-wal\n",
		} {
			man := []evidence.ManifestRecord{db, fsArt("E2", "extract", "IMG", 1, bad, nil)}
			r := FindSiblings(man, db)
			if r.WAL.Status != SiblingNone {
				t.Errorf("%s: %q was paired as the WAL: %+v", kind, bad, r.WAL)
			}
		}
		dbSp := fsArt("E3", kind, "IMG", 1, "/data/y.db ", nil)
		man := []evidence.ManifestRecord{dbSp, fsArt("E4", "extract", "IMG", 1, "/data/y.db-wal", nil)}
		if r := FindSiblings(man, dbSp); r.WAL.Status != SiblingNone {
			t.Errorf("%s: a database path with a trailing space was trimmed: %+v", kind, r.WAL)
		}
		good := []evidence.ManifestRecord{db, fsArt("E2", "extract", "IMG", 1, "/data/x.db-wal", nil)}
		if r := FindSiblings(good, db); r.WAL.Status != SiblingFound {
			t.Errorf("%s: exact name not found: %+v", kind, r.WAL)
		}
	}
}

func TestSiblingListsAreCappedInText(t *testing.T) {
	db := pulled("A0", "d1", "acq1", "x.db", "/x.db")
	man := []evidence.ManifestRecord{db}
	for i := 1; i <= 25; i++ {
		man = append(man, pulled(fmt.Sprintf("W%02d", i), "d1", "acq1", fmt.Sprintf("w%d", i), "/x.db-wal"))
	}
	r := FindSiblings(man, db)
	if len(r.WAL.Candidates) != 25 {
		t.Fatalf("the list itself must stay whole: %d", len(r.WAL.Candidates))
	}
	txt := r.WAL.Text("wal")
	if !strings.Contains(txt, "W20") || strings.Contains(txt, "W21") || !strings.Contains(txt, "5 more") {
		t.Errorf("text %q", txt)
	}
	if err := r.WAL.Err("wal").Error(); strings.Contains(err, "W21") || !strings.Contains(err, "5 more") {
		t.Errorf("err %q", err)
	}
	other := pulled("B0", "d1", "acqB", "x.db", "/x.db")
	om := []evidence.ManifestRecord{other}
	for i := 1; i <= 21; i++ {
		om = append(om, pulled(fmt.Sprintf("O%02d", i), "d1", "acqA", fmt.Sprintf("o%d", i), "/x.db-wal"))
	}
	ot := FindSiblings(om, other).WAL.Text("wal")
	if !strings.Contains(ot, "O20") || strings.Contains(ot, "O21") || !strings.Contains(ot, "1 more") {
		t.Errorf("other-acquisition text %q", ot)
	}
}

func TestSiblingCandidateKindsFollowEligibleKind(t *testing.T) {
	// A kind that is not database input is never a candidate, whatever its
	// identity fields say.
	db := pulled("A1", "d1", "acq1", "x.db", "/x.db")
	bad := pulled("A2", "d1", "acq1", "x.db-wal", "/x.db-wal")
	bad.Source.Kind = "unallocated"
	if r := FindSiblings([]evidence.ManifestRecord{db, bad}, db); r.WAL.Status != SiblingNone {
		t.Errorf("%+v", r.WAL)
	}
	// A file database pairs with a backup-kind companion (same class), not with an extract.
	bk := pulled("A3", "d1", "acq1", "x.db-wal", "/x.db-wal")
	bk.Source.Kind = "backup"
	if r := FindSiblings([]evidence.ManifestRecord{db, bk}, db); r.WAL.Status != SiblingFound {
		t.Errorf("%+v", r.WAL)
	}
}
