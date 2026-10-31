package ext4_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// contractForgedIDs are ids that exist nowhere: non-canonical forms (no read is allowed), and a canonical inode id
// past the inode count.
func contractForgedIDs(f *ext4.FS) []string {
	info := f.Info()
	blocks := info.Size / int64(info.BlockSize)
	return []string{
		"", "dirent:", "dirent:01:4", "dirent:+1:4", "dirent:1:-4", "dirent:1", "dirent:1:4:8", "dirent:5:3",
		fmt.Sprintf("dirent:%d:0", blocks), "dirent:18446744073709551616:0", "dirent:inline:", "dirent:inline:12:3",
		"dirent:inline:+12:4", "inode:07", "inode:+7", "inode:4294967295", "nid:7", " dirent:5:4",
	}
}

func TestRecovererContractOnExt4(t *testing.T) {
	rows := []struct {
		name string
		o    ext4test.Options
		data []byte
	}{
		{"extents-csum-4k", ext4test.Options{BlockSize: 4096, Extents: true, MetadataCsum: true}, pattern(9*4096+100, 31)},
		{"blockmap-1k", ext4test.Options{}, pattern(20*1024, 32)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			files := []ext4test.File{
				{Path: "/live.bin", Data: []byte("live")},
				{Path: "/gone.bin", Data: row.data, Deleted: true},
				{Path: "/a", Dir: true},
				{Path: "/a/a1", Data: []byte("a1")},
				{Path: "/b", Dir: true},
				{Path: "/b/b1", Data: []byte("b1")},
				{Path: "/b/bgone", Data: []byte("bg"), Deleted: true},
			}
			img, l := ext4test.BuildLayout(row.o, files)
			l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
			l.ShareDirBlock(img, "/a", "/b")
			fsys, rr := openLoc(t, img)
			var live filesys.Entry
			for _, e := range readDir(t, fsys, "/") {
				if e.Name == "live.bin" && !e.Deleted {
					live = e
				}
			}
			if live.ID == "" {
				t.Fatal("no live entry")
			}
			g := l.File("/b/bgone")
			dup := filesys.Entry{ID: direntIDAt(g.DirentBlock, g.DirentOffset), Deleted: true, Name: "bgone"}
			fstest.RecovererContract(t, fstest.RecovererSubject{
				FS:        fsys,
				Deleted:   delEntry(t, fsys, "/gone.bin"),
				Live:      live,
				ForgedIDs: contractForgedIDs(fsys),
				Reads:     rr.calls.Load,
				Duplicate: &dup,
			})
		})
	}
}

// A canonical dirent id naming a block no live directory maps is not found, and costs at most one index build plus one
// block read.
func TestRecoverableCanonicalUnknownIDIsNotFoundAndBounded(t *testing.T) {
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, append(capFiles(), ext4test.File{Path: "/data.bin", Data: pattern(3000, 33)}))
	fresh, _ := openLoc(t, img)
	g := l.File("/d3/gone")
	if _, _, _, err := fresh.ResolveDeleted(direntIDAt(g.DirentBlock, g.DirentOffset)); err != nil {
		t.Fatal(err)
	}
	index := fresh.LocateWork() // the index build plus one block

	f, _ := openLoc(t, img)
	blk := l.File("/data.bin").Data[0]
	e := filesys.Entry{ID: direntIDAt(blk, 0), Deleted: true}
	if cs, err := f.Recoverable(e); !errors.Is(err, filesys.ErrNotFound) || cs != nil {
		t.Fatalf("a data block id: %v, %v; want ErrNotFound", cs, err)
	}
	first := f.LocateWork()
	if first > index {
		t.Errorf("the first unknown id cost %d, above one index build plus a block (%d)", first, index)
	}
	for range 49 {
		if _, err := f.Recoverable(e); !errors.Is(err, filesys.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if got := f.LocateWork() - first; got > 49 {
		t.Errorf("49 more unknown ids added %d work, want at most 49: the directory index is built once", got)
	}
}
