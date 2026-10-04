package hfsplus_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

// TestDuplicateCNIDEntryIsConflictAndNeverOpens: two catalog records claim one
// catalog node id. The thread of that id names only one of them; the other is
// flagged cnid_conflict, gets an ID that cannot be resolved, and Open of it is a
// CorruptError, never the bytes of the record the thread names.
func TestDuplicateCNIDEntryIsConflictAndNeverOpens(t *testing.T) {
	aData := bytes.Repeat([]byte("A"), 100)
	bData := bytes.Repeat([]byte("B"), 100)
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/a.txt", Data: aData},
		{Path: "/b.txt", Data: bData},
	})
	aID := lay.CNIDs["/a.txt"]
	p := findRec(t, img, lay, 2, units("b.txt"))
	be.PutUint32(img[p.data+8:], aID) // b's file record now claims a's id
	f := open(t, img)

	root := readDir(t, f, f.Root())
	a, b := child(t, root, "a.txt"), child(t, root, "b.txt")
	if a.ID != idOf(aID) {
		t.Errorf("a.txt ID = %s, want %s", a.ID, idOf(aID))
	}
	if _, ok := attr(a, "cnid_conflict"); ok {
		t.Errorf("a.txt, the record the thread names, is flagged: %+v", a.Attrs)
	}
	if v, _ := attr(b, "cnid_conflict"); v != "true" {
		t.Fatalf("b.txt is not flagged cnid_conflict: %+v", b.Attrs)
	}
	if b.ID == a.ID {
		t.Errorf("b.txt shares the ID %s with a.txt", b.ID)
	}
	if !hasWarning(f.Info(), `"b.txt"`) || !hasWarning(f.Info(), "catalog node id") {
		t.Errorf("warnings = %q", f.Info().Warnings)
	}
	// Opening a.txt returns a.txt; b.txt never returns a.txt's bytes.
	fl, err := f.Open(a)
	if err != nil || !bytes.Equal(readAll(t, fl), aData) {
		t.Errorf("Open(a.txt) = %v, %v", fl, err)
	}
	for _, e := range []filesys.Entry{b, lookupEntry(t, f, "/b.txt")} {
		fl, err := f.Open(e)
		wantCorrupt(t, err)
		if fl != nil {
			t.Error("Open of the conflicting entry returned a file")
		}
	}
	// Forging fields of the entry changes nothing: only the ID counts.
	forged := b
	forged.ID = a.ID
	if fl, err := f.Open(forged); err != nil || !bytes.Equal(readAll(t, fl), aData) {
		t.Errorf("Open by the owner's ID = %v, %v", fl, err)
	}
}

// TestDuplicateCNIDFolderIsConflictAndNeverListed: a folder record that claims
// another folder's id is flagged and its ReadDir is a CorruptError; it never
// lists the other folder's children.
func TestDuplicateCNIDFolderIsConflictAndNeverListed(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/d1", Dir: true},
		{Path: "/d1/in1"},
		{Path: "/d2", Dir: true},
		{Path: "/d2/in2"},
	})
	p := findRec(t, img, lay, 2, units("d2"))
	be.PutUint32(img[p.data+8:], lay.CNIDs["/d1"]) // folder record: folderID at offset 8
	f := open(t, img)
	root := readDir(t, f, f.Root())
	d1, d2 := child(t, root, "d1"), child(t, root, "d2")
	if _, ok := attr(d1, "cnid_conflict"); ok {
		t.Errorf("d1 is flagged: %+v", d1.Attrs)
	}
	if v, _ := attr(d2, "cnid_conflict"); v != "true" {
		t.Fatalf("d2 is not flagged: %+v", d2.Attrs)
	}
	es, err := f.ReadDir(d2)
	wantCorrupt(t, err)
	if es != nil {
		t.Errorf("ReadDir of the conflicting folder listed %v", nameList(es))
	}
	if _, err := f.Lookup("/d2/in2"); err == nil {
		t.Error("Lookup went through the conflicting folder")
	}
}

// TestConflictIDsAreStrict: a forged conflict ID is never resolved to content;
// malformed spellings are ErrNotFound.
func TestConflictIDsAreStrict(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/a.txt", Data: []byte("x")}})
	for _, id := range []string{"conflict:", "conflict:16", "conflict:016:2:AGE", "conflict:16:2:A", "conflict:16:2:AA", "conflict:16:2:AGE=", "conflict:5:2:AGE", "conflict:16:-2:AGE", "conflict:99999999999:2:AGE", "conflict:16:02:AGE"} {
		if fl, err := f.Open(filesys.Entry{ID: id}); fl != nil || !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Open(%q) = %v, %v; want ErrNotFound", id, fl, err)
		}
		if _, err := f.ReadDir(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("ReadDir(%q) = %v; want ErrNotFound", id, err)
		}
	}
	for _, id := range []string{"conflict:16:2:AGE", "conflict:16:2:"} {
		if fl, err := f.Open(filesys.Entry{ID: id}); fl != nil {
			t.Errorf("Open(%q) returned a file", id)
		} else {
			wantCorrupt(t, err)
		}
	}
}

// TestDecmpfsAttributeWithoutFlagIsIgnored: the OS decides compression from
// UF_COMPRESSED; a com.apple.decmpfs attribute on a file without it does not
// replace the data fork. The data fork is the content, flagged decmpfs_ignored,
// with a warning.
func TestDecmpfsAttributeWithoutFlagIsIgnored(t *testing.T) {
	genuine := []byte("REAL DATA FORK BYTES")
	fake := []byte("FAKE-FROM-XATTR")
	files := []hfsplustest.File{{
		Path: "/f", Data: genuine,
		Attrs: []hfsplustest.Attr{{Name: nameDecmpfs, Value: decmpfsValue(3, uint64(len(fake)), zlibBytes(fake))}},
	}}
	_, _, f := buildTree(t, hfsplustest.Options{}, files)
	e, fl := openPath(t, f, "/f")
	if e.Size != int64(len(genuine)) || !bytes.Equal(readAll(t, fl), genuine) {
		t.Errorf("entry size %d, content %q; want the data fork", e.Size, readAll(t, fl))
	}
	if v, _ := attr(e, "decmpfs_ignored"); v != "true" {
		t.Errorf("attrs = %+v", e.Attrs)
	}
	if _, ok := attr(e, "compressed"); ok {
		t.Errorf("an unflagged file is shown compressed: %+v", e.Attrs)
	}
	if !hasWarning(f.Info(), "decmpfs") || !hasWarning(f.Info(), `"f"`) {
		t.Errorf("warnings = %q", f.Info().Warnings)
	}
}

func lookupEntry(t testing.TB, f *hfsplus.FS, p string) filesys.Entry {
	t.Helper()
	e, err := f.Lookup(p)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", p, err)
	}
	return e
}
