package hfsplus_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

func TestOpenFileWithoutThreadByCatalogScan(t *testing.T) {
	data := bytes.Repeat([]byte("legacy"), 100)
	_, lay, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", NoThread: true, Data: data},
		{Path: "/normal", Data: []byte("normal")},
	})
	root := readDir(t, f, f.Root())
	e := child(t, root, "legacy")
	if v, _ := attr(e, "thread"); v != "missing" {
		t.Fatalf("the listing must still flag thread=missing: %+v", e.Attrs)
	}
	if e.ID != idOf(lay.CNIDs["/legacy"]) {
		t.Fatalf("ID = %s, want %s", e.ID, idOf(lay.CNIDs["/legacy"]))
	}
	scanBefore, dirBefore := f.IDScanBudget(), f.DirBudget()
	fl, err := f.Open(e)
	if err != nil {
		t.Fatalf("Open of a file without a thread: %v", err)
	}
	if !bytes.Equal(readAll(t, fl), data) {
		t.Error("content differs")
	}
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Errorf("runs: %v", err)
	}
	// The scan, and only the scan, was charged, and to its own budget.
	if f.IDScanBudget() >= scanBefore {
		t.Errorf("the id scan was not charged: budget %d before, %d after", scanBefore, f.IDScanBudget())
	}
	if f.DirBudget() != dirBefore {
		t.Errorf("the id scan spent directory read budget: %d before, %d after", dirBefore, f.DirBudget())
	}
	// Open by the bare ID alone (the contract: nothing else of e is trusted).
	if fl2, err := f.Open(filesys.Entry{ID: e.ID}); err != nil || !bytes.Equal(readAll(t, fl2), data) {
		t.Errorf("Open by ID alone = %v, %v", fl2, err)
	}
}

// A file that has a thread never scans, even when another record claims its id.
func TestThreadPresentNeverScans(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/a.txt", Data: bytes.Repeat([]byte("A"), 100)},
		{Path: "/b.txt", Data: bytes.Repeat([]byte("B"), 100)},
	})
	aID := lay.CNIDs["/a.txt"]
	p := findRec(t, img, lay, 2, units("b.txt"))
	be.PutUint32(img[p.data+8:], aID) // b's file record now claims a's id; a's thread still names a.txt
	f := open(t, img)
	before := f.IDScanBudget()
	fl, err := f.Open(filesys.Entry{ID: idOf(aID)})
	if err != nil || !bytes.Equal(readAll(t, fl), bytes.Repeat([]byte("A"), 100)) {
		t.Fatalf("Open(a.txt) = %v, %v; the thread names a.txt", fl, err)
	}
	if f.IDScanBudget() != before {
		t.Error("an id with a thread must not scan the catalog")
	}
}

func TestNoThreadIDBeyondNextCNIDDoesNotScan(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/legacy", NoThread: true, Data: []byte("x")}})
	before := f.IDScanBudget()
	if _, err := f.Open(filesys.Entry{ID: "cnid:4294967000"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("an id the volume never issued: %v, want ErrNotFound", err)
	}
	if f.IDScanBudget() != before {
		t.Error("an id past nextCatalogID must be refused without reading the catalog")
	}
}

// A thread-less FOLDER id is not a file: ErrUnsupported, as for a folder with a thread.
func TestNoThreadFolderIDIsNotAFile(t *testing.T) {
	_, lay, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/nodir", Dir: true, NoThread: true}})
	if _, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/nodir"])}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open of a thread-less folder id: %v, want ErrUnsupported", err)
	}
	if _, err := f.ReadDir(filesys.Entry{ID: idOf(lay.CNIDs["/nodir"])}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("ReadDir of a thread-less folder keeps its documented behavior (unchanged by this task): %v", err)
	}
}

// Two thread-less records that share an id: the scan finds both and opens neither.
func TestNoThreadDuplicateIDIsConflict(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/a.txt", NoThread: true, Data: bytes.Repeat([]byte("A"), 100)},
		{Path: "/b.txt", NoThread: true, Data: bytes.Repeat([]byte("B"), 100)},
	})
	aID := lay.CNIDs["/a.txt"]
	p := findRec(t, img, lay, 2, units("b.txt"))
	be.PutUint32(img[p.data+8:], aID) // b's file record now claims a's id
	f := open(t, img)
	fl, err := f.Open(filesys.Entry{ID: idOf(aID)})
	wantCorrupt(t, err)
	if fl != nil {
		t.Error("a shared id must open neither record")
	}
	if !hasWarning(f.Info(), "shared by several records") {
		t.Errorf("no warning about the shared id: %q", f.Info().Warnings)
	}
}

// A record the scan cannot decode might be the second owner of the id, so
// uniqueness cannot be proven: the open fails closed instead of skipping it.
func TestNoThreadDamagedRecordMakesTheIDUnprovable(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", NoThread: true, Data: []byte("legacy")},
		{Path: "/d", Dir: true},
	})
	p := findRec(t, img, lay, 2, units("d"))
	be.PutUint16(img[p.data:], 2) // a folder record's type becomes "file": 88 bytes cannot hold a file record
	f := open(t, img)
	fl, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/legacy"])})
	wantCorrupt(t, err)
	if fl != nil {
		t.Error("an unprovable id must not open")
	}
}

// The scan has a budget of its own. When it ends a scan, each refused open is a
// typed error, is counted and is warned about; directory listings are untouched.
func TestNoThreadScanBudget(t *testing.T) {
	_, lay, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", NoThread: true, Data: []byte("x")},
		{Path: "/legacy2", NoThread: true, Data: []byte("y")},
		{Path: "/normal", Data: []byte("n")},
	})
	f.SetIDScanBudget(1)
	for _, p := range []string{"/legacy", "/legacy2"} {
		fl, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs[p])})
		wantCorrupt(t, err)
		if fl != nil {
			t.Errorf("%s: an unproven id opened", p)
		}
	}
	if f.IDScanRefused() != 2 {
		t.Errorf("%d refused scans counted, want 2", f.IDScanRefused())
	}
	if !hasWarning(f.Info(), "thread-less") {
		t.Errorf("no warning about the refused scans: %q", f.Info().Warnings)
	}
	// Everything else still works: ordinary listings and files with threads.
	root := readDir(t, f, f.Root())
	if _, err := f.Open(child(t, root, "normal")); err != nil {
		t.Errorf("a file with a thread must open after the scan budget is spent: %v", err)
	}
}

// A record of an unknown type has an unreadable id: it may be a second owner of the id
// being opened, so the open cannot prove uniqueness and fails closed. Listings only skip it.
func TestNoThreadUnknownRecordTypeMakesTheIDUnprovable(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", NoThread: true, Data: []byte("legacy")},
		{Path: "/other", Data: []byte("other")},
	})
	p := findRec(t, img, lay, 2, units("other"))
	be.PutUint16(img[p.data:], 0x00FF) // a record type no reader knows
	f := open(t, img)
	fl, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/legacy"])})
	wantCorrupt(t, err)
	if fl != nil {
		t.Error("an unprovable id must not open")
	}
	if !hasWarning(f.Info(), "uniqueness unprovable") {
		t.Errorf("no warning about the unknown record: %q", f.Info().Warnings)
	}
	// Listings keep skipping it with a warning.
	root := readDir(t, f, f.Root())
	for _, e := range root {
		if e.Name == "other" {
			t.Error("the unknown-type record was listed")
		}
	}
	if !hasWarning(f.Info(), "unknown type") {
		t.Errorf("the listing did not warn: %q", f.Info().Warnings)
	}
}

// The record cap ends a scan before the catalog does: a counted, typed refusal, never a miss.
func TestNoThreadScanRecordCap(t *testing.T) {
	_, lay, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/a", Data: []byte("a")},
		{Path: "/b", Data: []byte("b")},
		{Path: "/legacy", NoThread: true, Data: []byte("x")},
	})
	f.SetDirRecordCap(1)
	fl, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/legacy"])})
	wantCorrupt(t, err)
	if fl != nil {
		t.Error("an unproven id opened")
	}
	if f.IDScanRefused() != 1 {
		t.Errorf("%d refused scans counted, want 1", f.IDScanRefused())
	}
	if !hasWarning(f.Info(), "thread-less") {
		t.Errorf("no warning about the refused scan: %q", f.Info().Warnings)
	}
}

// An empty catalog (depth 0) holds no record of any id: ErrNotFound, never a default.
func TestNoThreadEmptyCatalogIsNotFound(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", NoThread: true, Data: []byte("x")},
	})
	h := lay.CatalogOffset(0) + 14 // the header record: depth, root, records, first leaf, last leaf
	be.PutUint16(img[h:], 0)
	be.PutUint32(img[h+2:], 0)
	be.PutUint32(img[h+10:], 0)
	be.PutUint32(img[h+14:], 0)
	f := open(t, img)
	_, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/legacy"])})
	if !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Open on an empty catalog: %v, want ErrNotFound", err)
	}
}

// Only the opened id's own thread position (id, empty name) may be skipped. An empty-name
// record under another parent can be a file or folder record (shown in the raw form) sharing the id.
func TestNoThreadForeignEmptyNameUnknownRecordFailsClosed(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", NoThread: true, Data: []byte("legacy")},
		{Path: "/d", Dir: true},
	})
	p := findRec(t, img, lay, lay.CNIDs["/d"], nil) // the folder's thread, keyed (its id, empty name)
	be.PutUint16(img[p.data:], 0x00FF)
	f := open(t, img)
	fl, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/legacy"])})
	wantCorrupt(t, err)
	if fl != nil {
		t.Error("an unprovable id must not open")
	}
	if !hasWarning(f.Info(), "uniqueness unprovable") {
		t.Errorf("no warning: %q", f.Info().Warnings)
	}
	if hasWarning(f.Info(), "skipped") {
		t.Errorf("a record that fails the open closed must not also be reported as skipped: %q", f.Info().Warnings)
	}
}

// A record keyed (the opened id, a NON-empty name) is not the id's own thread position: with an
// unknown type its id is unreadable and uniqueness is unprovable. Placed through the builder as a
// child of a thread-less folder, then retyped in the image bytes.
func TestNoThreadOwnParentNonEmptyNameUnknownRecordFailsClosed(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", Dir: true, NoThread: true},
		{Path: "/legacy/kid", Data: []byte("kid")},
	})
	p := findRec(t, img, lay, lay.CNIDs["/legacy"], units("kid"))
	be.PutUint16(img[p.data:], 0x00FF)
	f := open(t, img)
	fl, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/legacy"])})
	wantCorrupt(t, err)
	if fl != nil {
		t.Error("an unprovable id must not open")
	}
	if !hasWarning(f.Info(), "uniqueness unprovable") {
		t.Errorf("no warning: %q", f.Info().Warnings)
	}
	if hasWarning(f.Info(), "skipped") {
		t.Errorf("a record that fails the open closed must not also be reported as skipped: %q", f.Info().Warnings)
	}
}
