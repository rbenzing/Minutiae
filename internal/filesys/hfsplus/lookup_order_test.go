package hfsplus_test

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

// TestLookupAliasNeedsNoFoldScan: on a case-insensitive HFS+ volume the order
// is exact > display alias > case fold, so a "~raw~" alias needs one descent
// and no folder scan: it resolves with the directory budget spent, and an
// entry whose literal name differs from the alias only by case does not
// shadow the raw-named entry the alias denotes.
func TestLookupAliasNeedsNoFoldScan(t *testing.T) {
	alias, _ := rawForm(units("a/b"))
	rest := alias[len("~raw~"):]
	shadow := "~RAW~" + rest // folds equal to the alias text
	files := []hfsplustest.File{
		{Path: "/slash", NameUnits: units("a/b")},
		{Path: "/shadow", NameUnits: units(shadow)},
		{Path: "/plain"},
	}
	_, lay, f := buildTree(t, hfsplustest.Options{}, files) // H+: case-insensitive
	if hfsplusCaseSensitive(f) {
		t.Fatal("test needs a case-insensitive volume")
	}
	f.SetDirBudget(0) // a fold scan could not read a single leaf now
	before := f.DirBudget()
	e, err := f.Lookup("/" + alias)
	if err != nil || e.ID != idOf(lay.CNIDs["/slash"]) {
		t.Fatalf("Lookup(alias) = %+v, %v; want the raw-named entry %s", e, err, idOf(lay.CNIDs["/slash"]))
	}
	if f.DirBudget() != before {
		t.Errorf("the alias lookup spent %d bytes of the directory budget", before-f.DirBudget())
	}
	// The literal spelling still finds its own entry (exact first).
	e, err = f.Lookup("/" + shadow)
	if err != nil || e.ID != idOf(lay.CNIDs["/shadow"]) {
		t.Errorf("Lookup(%q) = %+v, %v", shadow, e, err)
	}
	// Alias spelled with the case of the shadow's prefix but not exactly: the alias is canonical text only,
	// so this is a plain name that folds onto the shadow entry.
	if e, err := f.Lookup("/~Raw~" + rest); err != nil || e.ID != idOf(lay.CNIDs["/shadow"]) {
		t.Errorf("fold-equal literal = %+v, %v", e, err)
	}
	// With the budget spent a name that only the fold scan could find is an error, not a wrong answer.
	if _, err := f.Lookup("/PLAIN"); err != nil && !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("fold lookup with no budget: %v", err)
	}
}

func hfsplusCaseSensitive(f interface{ Info() filesys.Info }) bool {
	for _, ft := range f.Info().Features {
		if ft == "case-sensitive" {
			return true
		}
	}
	return false
}

// TestDirRecordCapCountsThreadRecords: thread records are not entries but they
// are catalog records, so a forged range of them is bounded by the record cap.
func TestDirRecordCapCountsThreadRecords(t *testing.T) {
	thread := []byte{0, 3, 0, 0, 0, 0, 0, 1, 0, 0} // a folder thread: parent 1, empty name
	var raw []hfsplustest.RawRecord
	for i := range 50 {
		raw = append(raw, hfsplustest.RawRecord{Parent: 16, Name: units(fmt.Sprintf("t%02d", i)), Data: thread})
	}
	files := []hfsplustest.File{{Path: "/d", Dir: true}, {Path: "/d/x1"}, {Path: "/d/x2"}}
	_, _, f := buildTree(t, hfsplustest.Options{RawRecords: raw}, files)
	f.SetDirRecordCap(10)
	es, err := f.ReadDir(filesys.Entry{ID: "cnid:16"})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 0 || !hasWarning(f.Info(), "more than 10 catalog records") {
		t.Errorf("%d entries, warnings %q; the thread records must hit the record cap", len(es), f.Info().Warnings)
	}
}

func TestDuplicateCNIDInListingIsWarned(t *testing.T) {
	rec := make([]byte, 248)
	be.PutUint16(rec[0:], 2) // a file record
	be.PutUint32(rec[8:], 16)
	_, _, f := buildTree(t, hfsplustest.Options{RawRecords: []hfsplustest.RawRecord{{Parent: 2, Name: units("dup"), Data: rec}}},
		[]hfsplustest.File{{Path: "/a"}})
	es := readDir(t, f, f.Root())
	if len(es) != 2 || es[0].ID != es[1].ID {
		t.Fatalf("entries = %v", nameList(es))
	}
	if !hasWarning(f.Info(), "more than once") {
		t.Errorf("warnings = %q", f.Info().Warnings)
	}
}

func TestTruncatedInsideHeaderNodeIsCorrupt(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, sampleTree())
	cut := lay.CatalogOffset(100) // the image ends inside node 0 of the catalog
	f := open(t, img[:cut])
	_, err := f.Lookup("/docs")
	wantCorrupt(t, err)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		t.Errorf("a short header read is reported as a bare EOF: %v", err)
	}
}

func TestThreadInvalidIsFlagged(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/f", Data: []byte("x")}})
	p := findRec(t, img, lay, lay.CNIDs["/f"], nil)
	be.PutUint16(img[p.data:], 2) // a "file record" where the thread is: far too short
	f := open(t, img)
	e := child(t, readDir(t, f, f.Root()), "f")
	if v, _ := attr(e, "thread"); v != "invalid" {
		t.Errorf("attrs = %+v", e.Attrs)
	}
	if !hasWarning(f.Info(), "thread record") {
		t.Errorf("warnings = %q", f.Info().Warnings)
	}
}

func TestRootEntryIsNotSharedWithCallers(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{}, sampleTree())
	a := f.Root()
	if len(a.Attrs) == 0 {
		t.Fatal("the root has no attributes")
	}
	a.Attrs[0].Value = "tampered"
	a.Attrs = append(a.Attrs, filesys.KV{Key: "x", Value: "y"})
	b := f.Root()
	if b.Attrs[0].Value == "tampered" || len(b.Attrs) == len(a.Attrs) {
		t.Errorf("a caller's change reached the cached root entry: %+v", b.Attrs)
	}
}
