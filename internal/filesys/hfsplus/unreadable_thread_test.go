package hfsplus_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

// An unknown-type record AT the thread position (id, empty name) means the thread is
// unreadable, not missing: ReadDir by id is a CorruptError, never ErrNotFound.
func TestReadDirUnreadableThreadIsCorrupt(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/d", Dir: true}})
	p := findRec(t, img, lay, lay.CNIDs["/d"], nil)
	be.PutUint16(img[p.data:], 0x00FF)
	f := open(t, img)
	_, err := f.ReadDir(filesys.Entry{ID: idOf(lay.CNIDs["/d"])})
	if errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("an unreadable thread was reported as missing: %v", err)
	}
	wantCorrupt(t, err)
	if !strings.Contains(err.Error(), "255") {
		t.Errorf("the error does not name the record type: %v", err)
	}
}

// A lookup that walks past an unknown-type record reports it: the thread of a thread-less
// folder is missing, and the child record the lookup passed is warned about, never silent.
func TestReadDirByIDWarnsOnPassedUnknownRecord(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", Dir: true, NoThread: true},
		{Path: "/legacy/kid", Data: []byte("kid")},
	})
	p := findRec(t, img, lay, lay.CNIDs["/legacy"], units("kid"))
	be.PutUint16(img[p.data:], 0x00FF)
	f := open(t, img)
	_, err := f.ReadDir(filesys.Entry{ID: idOf(lay.CNIDs["/legacy"])})
	if !errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("ReadDir = %v, want ErrNotFound (no thread at all)", err)
	}
	if !hasWarning(f.Info(), "unknown type 255") {
		t.Errorf("the lookup passed an unknown-type record silently: %q", f.Info().Warnings)
	}
}

// toEntry: a file whose thread is unreadable is flagged unreadable, never "missing".
func TestEntryUnreadableThreadIsNotMissing(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/f", Data: []byte("f")},
		{Path: "/g", Data: []byte("g")},
	})
	p := findRec(t, img, lay, lay.CNIDs["/f"], nil)
	be.PutUint16(img[p.data:], 0x00FF)
	f := open(t, img)
	e := child(t, readDir(t, f, f.Root()), "f")
	if v, _ := attr(e, "thread"); v != "unreadable" {
		t.Errorf("thread attribute = %q, want unreadable: %+v", v, e.Attrs)
	}
	if !hasWarning(f.Info(), "unreadable") {
		t.Errorf("no warning about the unreadable thread: %q", f.Info().Warnings)
	}
}

// toEntry: the lookup of a thread-less file's thread passes the next id's unknown-type thread
// record and says so.
func TestEntryWarnsOnPassedUnknownRecord(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", NoThread: true, Data: []byte("legacy")},
		{Path: "/zz", Data: []byte("zz")},
	})
	if lay.CNIDs["/zz"] != lay.CNIDs["/legacy"]+1 {
		t.Fatalf("ids %d, %d are not consecutive", lay.CNIDs["/legacy"], lay.CNIDs["/zz"])
	}
	p := findRec(t, img, lay, lay.CNIDs["/zz"], nil)
	be.PutUint16(img[p.data:], 0x00FF)
	f := open(t, img)
	e := child(t, readDir(t, f, f.Root()), "legacy")
	if v, _ := attr(e, "thread"); v != "missing" {
		t.Errorf("thread attribute = %q, want missing", v)
	}
	if !hasWarning(f.Info(), "unknown type 255 under parent") {
		t.Errorf("the lookup passed an unknown-type record silently: %q", f.Info().Warnings)
	}
}

// The fail-closed open reports the record once: the lookup's skip warning is folded into the
// scan's "uniqueness unprovable" warning, which names the same record.
func TestOpenFoldsLookupWarningIntoUnprovable(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", NoThread: true, Data: []byte("legacy")},
		{Path: "/zz", Data: []byte("zz")},
	})
	p := findRec(t, img, lay, lay.CNIDs["/zz"], nil)
	be.PutUint16(img[p.data:], 0x00FF)
	f := open(t, img)
	fl, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/legacy"])})
	wantCorrupt(t, err)
	if fl != nil {
		t.Error("an unprovable id opened")
	}
	if hasWarning(f.Info(), "skipped") {
		t.Errorf("a skip warning on the fail-closed path: %q", f.Info().Warnings)
	}
	if n := countWarnings(f.Info(), "under parent "+strings.TrimPrefix(idOf(lay.CNIDs["/zz"]), "cnid:")); n != 1 {
		t.Errorf("%d warnings name the record, want exactly 1: %q", n, f.Info().Warnings)
	}
}

// A file whose OWN thread is unreadable opens when the scan proves the id unique (Y70).
func TestOpenOwnUnreadableThreadOpensWhenScanProvesUnique(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/f", Data: []byte("f")}})
	p := findRec(t, img, lay, lay.CNIDs["/f"], nil)
	be.PutUint16(img[p.data:], 0x00FF)
	f := open(t, img)
	fl, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/f"])})
	if err != nil || fl == nil {
		t.Fatalf("Open = %v, %v", fl, err)
	}
	if !hasWarning(f.Info(), "skipped") {
		t.Errorf("the skipped own thread was not reported: %q", f.Info().Warnings)
	}
}

// The thread-less FILE form of the own-parent test: another file's record, retyped to an unknown
// type and re-keyed under the opened id with a non-empty name, makes uniqueness unprovable.
func TestNoThreadFileOwnParentNonEmptyNameUnknownRecordFailsClosed(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", NoThread: true, Data: []byte("legacy")},
		{Path: "/other", Data: []byte("other")},
	})
	p := findRec(t, img, lay, 2, units("other"))
	be.PutUint16(img[p.data:], 0x00FF)
	be.PutUint32(img[p.key+2:], lay.CNIDs["/legacy"])
	f := open(t, img)
	fl, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/legacy"])})
	wantCorrupt(t, err)
	if fl != nil {
		t.Error("an unprovable id must not open")
	}
}

// An unknown-type record AT (id, empty name) is an unreadable thread, never "not found", whatever
// the volume header's next catalog id says (Y79).
func TestOpenUnreadableThreadBeyondNextCNIDIsCorrupt(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/f", Data: []byte("f")}})
	id := lay.CNIDs["/f"]
	p := findRec(t, img, lay, id, nil)
	be.PutUint16(img[p.data:], 0x00FF)
	be.PutUint32(img[1024+64:], 16) // nextCatalogID at the first user id, at or below id
	be.PutUint32(img[len(img)-1024+64:], 16)
	if id < 16 {
		t.Fatalf("id %d is below the first user id", id)
	}
	f := open(t, img)
	fl, err := f.Open(filesys.Entry{ID: idOf(id)})
	if errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("an unreadable thread was reported as never issued: %v", err)
	}
	wantCorrupt(t, err)
	if fl != nil {
		t.Error("an id with an unreadable thread opened")
	}
	if !strings.Contains(err.Error(), "255") {
		t.Errorf("the error does not name the record type: %v", err)
	}
}

// The fold absorbs at most ONE lookup record: a second record with the same key (scanFrom does
// not check strict key order) is warned about separately (Y80).
func TestOpenFoldAbsorbsOnlyOneDuplicateKeyRecord(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/legacy", NoThread: true, Data: []byte("legacy")},
		{Path: "/zz", Data: []byte("zz")},
		{Path: "/zy", Data: []byte("zy")},
	})
	zz, zy := lay.CNIDs["/zz"], lay.CNIDs["/zy"]
	a := findRec(t, img, lay, zz, nil)
	b := findRec(t, img, lay, zy, nil)
	be.PutUint16(img[a.data:], 0x00FF)
	be.PutUint16(img[b.data:], 0x00FF)
	be.PutUint32(img[b.key+2:], zz) // both records now carry the key (zz, empty name)
	f := open(t, img)
	fl, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/legacy"])})
	wantCorrupt(t, err)
	if fl != nil {
		t.Error("an unprovable id opened")
	}
	if n := countWarnings(f.Info(), "under parent "+strings.TrimPrefix(idOf(zz), "cnid:")); n != 2 {
		t.Errorf("%d warnings name the duplicated key, want 2 (one folded into the scan's, one for the duplicate): %q", n, f.Info().Warnings)
	}
}
