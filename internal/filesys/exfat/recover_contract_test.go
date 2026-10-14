package exfat_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// recReader counts the ReadAt calls made on the image.
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

func TestExfatRecovererContract(t *testing.T) {
	longName := "/" + strings.Repeat("n", 36) + ".bin" // 40 UTF-16 units: three name entries
	img, l := exfattest.BuildLayout(exfattest.Options{ClusterCount: 256}, []exfattest.File{
		{Path: "/LIVE.TXT", Data: []byte("live")},
		{Path: longName, Data: bytes.Repeat([]byte{7}, 5000), Deleted: true},
	})
	rr := &recReader{r: bytes.NewReader(img)}
	fsys, err := exfat.Open(rr, int64(len(img)))
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
	id := func(p string, deleted bool) string {
		loc := l.Find(p, deleted)
		return fmt.Sprintf("dirent:%d:%d", loc.DirCluster, loc.Index)
	}
	deleted, live := byID[id(longName, true)], byID[id("/LIVE.TXT", false)]
	if !deleted.Deleted || live.Deleted || live.ID == "" {
		t.Fatalf("listing: deleted %+v live %+v", deleted, live)
	}
	if sc := img[l.Find(longName, true).Offset+1]; sc != 4 {
		t.Fatalf("secondary count %d, want 4 (a stream and three name entries)", sc)
	}
	fstest.RecovererContract(t, fstest.RecovererSubject{
		FS: fsys, Deleted: deleted, Live: live,
		ForgedIDs: []string{
			"", "dirent:", "dirent:01:2", "dirent:+1:2", "dirent:1:-2", "dirent:4294967296:0",
			"dirent:1:4294967296", "dir:01", "nid:1",
		},
		Reads:         rr.count,
		SkipDuplicate: "an exFAT entry id is the position of the entry set; one position cannot be held by two objects",
	})
}

// TestExfatRecoverableCanonicalUnknownIDIsNotFoundAndBounded: a canonical id of a directory that does not
// exist searches the tree, bounded by MaxLocateScans listings, and is ErrNotFound.
func TestExfatRecoverableCanonicalUnknownIDIsNotFoundAndBounded(t *testing.T) {
	img := exfattest.Build(exfattest.Options{ClusterCount: 256}, []exfattest.File{
		{Path: "/D", Dir: true}, {Path: "/D/A.TXT", Data: []byte("a")}, {Path: "/B.TXT", Data: []byte("b")},
	})
	one := &recReader{r: bytes.NewReader(img)}
	f1, err := exfat.Open(one, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f1.ReadDir(f1.Root()); err != nil {
		t.Fatal(err)
	}
	perListing := one.calls.Load() // Open plus one listing of the root
	rr := &recReader{r: bytes.NewReader(img)}
	f2, err := exfat.Open(rr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := f2.Recoverable(filesys.Entry{ID: "dirent:5:4294967295"})
	if !errors.Is(err, filesys.ErrNotFound) || cs != nil {
		t.Fatalf("Recoverable = %v, %v; want nil and ErrNotFound", cs, err)
	}
	if got, limit := rr.calls.Load(), int64(exfat.MaxLocateScans+1)*perListing; got > limit {
		t.Errorf("%d reads for an unknown directory, limit %d (MaxLocateScans+1 listings of %d reads)", got, limit, perListing)
	}
}
