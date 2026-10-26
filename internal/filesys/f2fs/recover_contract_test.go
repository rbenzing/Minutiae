package f2fs_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// TestRecovererContractF2FS runs the shared Recoverer contract kit on builder
// images with an unlinked file (its stale inode and data in free node and data
// space): a plain volume, and one with inode checksums and extra attributes.
func TestRecovererContractF2FS(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    f2fstest.Options
	}{
		{"plain", f2fstest.Options{}},
		{"checksum-extra-attr", f2fstest.Options{InodeChksum: true, ExtraAttr: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, _ := f2fstest.BuildTree(recOpts(tc.o), []f2fstest.File{
				{Path: "/d/live.txt", Data: stamp(bs+5, "l")},
				{Path: "/d/gone.bin", Data: stamp(3*bs-7, "g"), Deleted: true, Unlinked: true},
			})
			cr := &countReader{r: bytes.NewReader(img)}
			fsys, err := f2fs.Open(cr, int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			d, err := fsys.Lookup("/d")
			if err != nil {
				t.Fatal(err)
			}
			list, err := fsys.ReadDir(d)
			if err != nil {
				t.Fatal(err)
			}
			var deleted, live filesys.Entry
			for _, e := range list {
				switch e.Name {
				case "gone.bin":
					deleted = e
				case "live.txt":
					live = e
				}
			}
			if !deleted.Deleted || live.Deleted || live.ID == "" {
				t.Fatalf("listing: deleted %+v live %+v", deleted, live)
			}
			fstest.RecovererContract(t, fstest.RecovererSubject{
				FS: fsys, Deleted: deleted, Live: live,
				ForgedIDs: []string{
					"dentry:03:0:1", "dentry:3:0", "dentry:3:0:1:2", "dentry:4294967296:0:0", "dentry:3:0:214",
					"dentry:1:0:0", "dentry:4294967295:0:0", "dentry:3:-1:0", "nid:", "",
				},
				Reads:         cr.n.Load,
				SkipDuplicate: "an F2FS dentry ID is the location of one slot; two objects cannot hold it",
			})
		})
	}
}

// A stale inode whose i_pino is the inode number of a REGULAR file, and that
// file's data holding a crafted deleted dentry: the regular file is not a
// directory, so a slot "read" from it is never a deleted record, although the
// forged parent and the name would otherwise match (T6 carry-over; it pins the
// directory-type check of resolveSlot).
func TestRecoverableForgedParentIsARegularFile(t *testing.T) {
	var forged string
	fsys, _, _ := recImage(t, f2fstest.Options{},
		[]f2fstest.File{
			{Path: "/f.bin", Data: stamp(bs, "f")},
			{Path: "/d/gone.bin", Data: stamp(bs, "g"), Deleted: true, Unlinked: true},
		},
		func(img []byte, tr *f2fstest.Tree) {
			o := recOpts(f2fstest.Options{})
			copy(blk(img, tr.Data["/f.bin"][0]), blk(img, tr.DirBlocks["/d"][0]))
			a := tr.Addr[tr.NID["/d/gone.bin"]]
			le.PutUint32(blk(img, a)[oPino:], tr.NID["/f.bin"])
			f2fstest.ResealInode(o, img, a)
			forged = fmt.Sprintf("dentry:%d:0:", tr.NID["/f.bin"])
		})
	realID := delID(t, fsys, "/d", "gone.bin")
	forged += realID[strings.LastIndex(realID, ":")+1:]
	if cs, err := fsys.Recoverable(filesys.Entry{ID: forged}); !errors.Is(err, filesys.ErrNotFound) || cs != nil {
		t.Errorf("%s: %v, %v; want nil and ErrNotFound", forged, cs, err)
	}
}

// A free dentry slot that names inode 0 or an inode beyond the NAT is not a
// deleted record: the listing never offers it, so Recoverable, asked for it,
// finds no deleted record there (ErrNotFound by the unlisted-slot rule) and
// never a candidate (T6 carry-over (c)).
func TestRecoverableSlotNamingInodeZeroOrBeyondNATIsNoRecord(t *testing.T) {
	for _, ino := range []uint32{0, 0x7fffffff} {
		var id string
		fsys, _, _ := recImage(t, f2fstest.Options{}, goneFile(stamp(bs, "g")), func(img []byte, tr *f2fstest.Tree) {
			dirBlk := blk(img, tr.DirBlocks["/d"][0])
			for s := range 214 { // the deleted slot is the one whose inode is gone.bin's
				if le.Uint32(dirBlk[34+s*11:]) == tr.NID["/d/gone.bin"] {
					le.PutUint32(dirBlk[34+s*11:], ino)
					id = fmt.Sprintf("dentry:%d:0:%d", tr.NID["/d"], s)
				}
			}
		})
		if id == "" {
			t.Fatal("the deleted slot was not found")
		}
		if cs, err := fsys.Recoverable(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) || cs != nil {
			t.Errorf("ino %d: %v, %v; want nil and ErrNotFound", ino, cs, err)
		}
	}
}
