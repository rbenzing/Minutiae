package fat_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// recReader counts the ReadAt calls made on the image (the repository has several package-private
// counting readers, none shared; this one also serves the fuzz and bound tests of this package).
type recReader struct {
	r     io.ReaderAt
	calls atomic.Int64
	bytes atomic.Int64
}

func (c *recReader) ReadAt(p []byte, off int64) (int, error) {
	c.calls.Add(1)
	n, err := c.r.ReadAt(p, off)
	c.bytes.Add(int64(n))
	return n, err
}

func (c *recReader) count() int64 { return c.calls.Load() }

func TestRecovererContractOnFAT(t *testing.T) {
	for _, typ := range []int{12, 16, 32} {
		t.Run(fmt.Sprintf("FAT%d", typ), func(t *testing.T) {
			img, b := fattest.BuildLayout(fattest.Options{Type: typ}, []fattest.File{
				{Path: "/LIVE.TXT", Data: []byte("live")},
				{Path: "/gone with a long name.bin", Data: bytes.Repeat([]byte{7}, 1500), LongName: true, Deleted: true},
			})
			rr := &recReader{r: bytes.NewReader(img)}
			fsys, err := fat.Open(rr, int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			list, err := fsys.ReadDir(fsys.Root())
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]filesys.Entry{}
			for _, e := range list {
				byID[e.ID] = e
			}
			deleted, live := byID[direntIDOf(b.Find("/gone with a long name.bin", true))], byID[direntIDOf(b.Find("/LIVE.TXT", false))]
			if !deleted.Deleted || live.Deleted || live.ID == "" {
				t.Fatalf("listing: deleted %+v live %+v", deleted, live)
			}
			fstest.RecovererContract(t, fstest.RecovererSubject{
				FS: fsys, Deleted: deleted, Live: live,
				ForgedIDs: []string{
					"", "dirent:", "dirent:01:2", "dirent:+1:2", "dirent:1:-2", "dirent:1", "dirent:1:2:3",
					"dirent:4294967296:0", "dirent:0:65536", "dirent:0:99999999999", "nid:7", " dirent:0:1",
				},
				Reads: rr.count,
				SkipDuplicate: "a FAT entry id is the position of the entry (directory cluster and index), so one id cannot be held by two objects; " +
					"a cross-linked directory is reachable from two listings, but each entry in it still has the one id dirent:<cluster>:<index>; TestRecoverCrossLinkedDirectories covers it",
			})
		})
	}
}

// TestRecoverableCanonicalUnknownIDIsNotFoundAndBounded: a canonical id of a directory that does not exist
// searches the tree, bounded by maxLocateScans listings (the export_test constant MaxLocateScans), and is
// ErrNotFound.
func TestRecoverableCanonicalUnknownIDIsNotFoundAndBounded(t *testing.T) {
	files := []fattest.File{{Path: "/D", Dir: true}, {Path: "/D/A.TXT", Data: []byte("a")}, {Path: "/B.TXT", Data: []byte("b")}}
	img := fattest.Build(fattest.Options{Type: 16}, files)
	// A fresh reader: the cost of Open and one listing of the root. Open already caches the root, so a
	// listing alone would cost 0 reads; the bound is therefore taken over Open plus the search.
	one := &recReader{r: bytes.NewReader(img)}
	f1, err := fat.Open(one, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f1.ReadDir(f1.Root()); err != nil {
		t.Fatal(err)
	}
	perListing := one.calls.Load()
	rr := &recReader{r: bytes.NewReader(img)}
	f2, err := fat.Open(rr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := f2.Recoverable(filesys.Entry{ID: "dirent:77:3"})
	if !errors.Is(err, filesys.ErrNotFound) || cs != nil {
		t.Fatalf("Recoverable = %v, %v; want nil and ErrNotFound", cs, err)
	}
	if got, limit := rr.calls.Load(), int64(fat.MaxLocateScans+1)*perListing; got > limit {
		t.Errorf("%d reads for an unknown directory, limit %d (MaxLocateScans+1 listings of %d reads)", got, limit, perListing)
	}
}

// TestRecoverCrossLinkedDirectories: two directory entries that share a first cluster list the same
// entries. The directory is one object under one id ("dir:<cluster>"), and a deleted file in it has the
// single id dirent:<cluster>:<index>, so Recoverable gives the same answer whichever listing the entry
// came from, and Walk reports the second appearance of the directory as a cycle instead of following it.
func TestRecoverCrossLinkedDirectories(t *testing.T) {
	for _, typ := range []int{12, 16, 32} {
		_, img, b := recImage(t, fattest.Options{Type: typ}, []fattest.File{
			{Path: "/D1", Dir: true},
			{Path: "/D2", Dir: true},
			{Path: "/D1/GONE.BIN", Data: bytes.Repeat([]byte{3}, 700), Deleted: true},
			{Path: "/D2/OTHER.BIN", Data: []byte("zz"), Deleted: true},
		})
		d1, d2 := b.Find("/D1", false), b.Find("/D2", false)
		fattest.PatchEntry(img, b.Geometry, d2, &d1.FirstCluster, nil) // D2 now starts where D1 starts
		fsys := reopen(t, img)
		root, err := fsys.ReadDir(fsys.Root())
		if err != nil {
			t.Fatal(err)
		}
		var answers [][]filesys.Candidate
		var ids []string
		for _, e := range root {
			if e.Name != "D1" && e.Name != "D2" {
				continue
			}
			kids, err := fsys.ReadDir(e)
			if err != nil {
				t.Fatalf("FAT%d: ReadDir(%s): %v", typ, e.Name, err)
			}
			for _, k := range kids {
				if !k.Deleted {
					continue
				}
				cs, err := fsys.Recoverable(k)
				if err != nil || len(cs) != 1 {
					t.Fatalf("FAT%d: Recoverable(%s via %s): %v, %d candidates", typ, k.ID, e.Name, err, len(cs))
				}
				answers = append(answers, cs)
				ids = append(ids, k.ID)
			}
		}
		if len(answers) != 2 || ids[0] != ids[1] || !reflect.DeepEqual(answers[0], answers[1]) {
			t.Errorf("FAT%d: ids %v, answers %+v; want one id and equal answers from both listings", typ, ids, answers)
		}
		cycles := 0
		_ = filesys.Walk(fsys, fsys.Root(), "/", func(_ string, _ filesys.Entry, err error) error {
			if errors.Is(err, filesys.ErrCorrupt) {
				cycles++
			}
			return nil
		})
		if cycles != 1 {
			t.Errorf("FAT%d: Walk reported %d cycles, want 1 (the second appearance of the shared directory)", typ, cycles)
		}
	}
}

// TestRecoverableScanCapIsUndecidableNotAbsent: when the search for a directory
// is cut off by the scan cap, the directory is undecidable, not absent. The
// error is the shared filesys.ErrLimit naming the cap and is never ErrNotFound.
func TestRecoverableScanCapIsUndecidableNotAbsent(t *testing.T) {
	files := []fattest.File{{Path: "/D1", Dir: true}, {Path: "/D1/D2", Dir: true}, {Path: "/D1/D2/D3", Dir: true}, {Path: "/B.TXT", Data: []byte("b")}}
	img := fattest.Build(fattest.Options{Type: 16}, files)
	f, err := fat.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	f.SetLocateScans(2)
	cs, err := f.Recoverable(filesys.Entry{ID: "dirent:77:3"})
	if cs != nil || !errors.Is(err, filesys.ErrLimit) || errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("Recoverable = %v, %v; want nil and filesys.ErrLimit (not ErrNotFound)", cs, err)
	}
	if !strings.Contains(err.Error(), "2") {
		t.Errorf("error %q does not name the cap", err)
	}
}

// TestRecoverableUnreadableSubdirectoryIsNotAbsent (F67): when a listing in the
// search failed and the target was not found, the subtree was unreadable: the
// answer is a typed error wrapping the failure, never ErrNotFound.
func TestRecoverableUnreadableSubdirectoryIsNotAbsent(t *testing.T) {
	files := []fattest.File{{Path: "/D1", Dir: true}, {Path: "/D1/D2", Dir: true}, {Path: "/D1/D2/F.TXT", Data: []byte("f")}}
	img, b := fattest.BuildLayout(fattest.Options{Type: 16}, files)
	d1 := b.Find("/D1", false)
	d2 := b.Find("/D1/D2", false)
	fr := &failingReader{r: bytes.NewReader(img)}
	off := b.Geometry.ClusterOffset(d1.FirstCluster)
	fr.lo, fr.hi = off, off+int64(b.Geometry.SectorSize*b.Geometry.SecPerClus)
	f, err := fat.Open(fr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	fr.armed.Store(true)
	cs, err := f.Recoverable(filesys.Entry{ID: fmt.Sprintf("dirent:%d:0", d2.FirstCluster)})
	if cs != nil || err == nil || errors.Is(err, filesys.ErrNotFound) || !strings.Contains(err.Error(), "injected read failure") {
		t.Fatalf("Recoverable = %v, %v; want nil and an error wrapping the read failure, not ErrNotFound", cs, err)
	}
}
