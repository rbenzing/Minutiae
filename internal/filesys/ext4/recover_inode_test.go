package ext4_test

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

const (
	testEAInodeFlag = 0x200000
	testInlineFlag  = 0x10000000
	testEncryptFlag = 0x800
)

// staleID returns the ID of the one deleted entry of the root directory named name (or, for a first-in-block record
// that has lost its inode number, the only deleted entry).
func staleID(t *testing.T, f *ext4.FS, name string) string {
	t.Helper()
	var only []string
	for _, e := range readDir(t, f, "/") {
		if !e.Deleted {
			continue
		}
		if e.Name == name {
			return e.ID
		}
		only = append(only, e.ID)
	}
	if len(only) == 1 {
		return only[0]
	}
	t.Fatalf("no deleted entry %q in the root", name)
	return ""
}

func putLE16(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
func putLE32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }

func orFlags(v uint32) func(raw []byte) {
	return func(raw []byte) { putLE32(raw, 0x20, binary.LittleEndian.Uint32(raw[0x20:])|v) }
}

func TestStaleInodeRules(t *testing.T) {
	csum := ext4test.Options{Extents: true, MetadataCsum: true, InlineData: true, Groups: 2, InodesPerGroup: 32}
	plain := ext4test.Options{Extents: true, InlineData: true, Groups: 2, InodesPerGroup: 32}
	gdt := ext4test.Options{Extents: true, GDTCsum: true, Groups: 2, InodesPerGroup: 32}
	gone := []ext4test.File{{Path: "/keep", Data: []byte("keep")}, {Path: "/gone", Data: []byte("deleted content"), Deleted: true}}
	patch := func(f func(raw []byte)) func(img []byte, l *ext4test.Layout) {
		return func(img []byte, l *ext4test.Layout) { l.PatchInode(img, l.File("/gone").Inode, f) }
	}

	type row struct {
		name  string
		opts  ext4test.Options
		files []ext4test.File
		// mutate runs after the default DebugfsRm unlink (unless raw is set, which skips the unlink).
		mutate  func(img []byte, l *ext4test.Layout)
		raw     bool
		wantErr error  // matched with errors.Is when set
		corrupt bool   // want a *filesys.CorruptError
		text    string // substring of the error text
		flags   uint32 // flag bits the accepted inode must have kept
	}
	rows := []row{
		{name: "plain deleted file is accepted", opts: csum, files: gone},
		{
			name: "live hard link (links 1, bitmap used)", opts: csum, files: gone, raw: true,
			mutate: patch(func(raw []byte) { putLE16(raw, 0x1A, 1); putLE32(raw, 0x14, 0) }), wantErr: filesys.ErrReused, text: "allocated",
		},
		{
			name: "bitmap re-set (reused)", opts: csum, files: gone,
			mutate: func(img []byte, l *ext4test.Layout) { l.SetInodeFree(img, l.File("/gone").Inode, false) }, wantErr: filesys.ErrReused, text: "allocated",
		},
		{name: "free with links 1", opts: csum, files: gone, mutate: patch(func(raw []byte) { putLE16(raw, 0x1A, 1) }), corrupt: true},
		{name: "free with dtime 0", opts: csum, files: gone, mutate: patch(func(raw []byte) { putLE32(raw, 0x14, 0) }), corrupt: true},
		{name: "dirent regular, inode directory", opts: csum, files: gone, mutate: patch(func(raw []byte) { putLE16(raw, 0, 0x41ED) }), wantErr: filesys.ErrReused},
		{name: "EA inode flag", opts: csum, files: gone, mutate: patch(orFlags(testEAInodeFlag)), corrupt: true},
		{name: "inline flag", opts: csum, files: gone, mutate: patch(orFlags(testInlineFlag)), wantErr: filesys.ErrUnsupported},
		{name: "fast symlink", opts: csum, files: []ext4test.File{{Path: "/keep", Data: []byte("k")}, {Path: "/gone", Symlink: "target", Deleted: true}}, wantErr: filesys.ErrUnsupported},
		{
			name: "inode checksum flipped", opts: csum, files: gone,
			mutate: func(img []byte, l *ext4test.Layout) { img[l.File("/gone").InodeOffset+0x7C] ^= 0xFF }, corrupt: true,
		},
		{
			name: "record inode 0", opts: plain,
			files:   []ext4test.File{{Path: "/keep", Data: []byte("k")}, {Path: "/gone", Data: []byte("g"), Deleted: true, NewBlock: true}},
			wantErr: filesys.ErrUndecided, text: "inode number",
		},
		{
			name: "inode in an INODE_UNINIT group", opts: gdt, files: gone,
			mutate: func(img []byte, l *ext4test.Layout) {
				l.PatchDescriptor(img, 0, func(d []byte) { putLE16(d, 0x12, testInodeUninit) })
			}, wantErr: filesys.ErrUndecided, text: "INODE_UNINIT",
		},
		{name: "encrypted inode keeps its flag", opts: csum, files: gone, mutate: patch(orFlags(testEncryptFlag)), flags: testEncryptFlag},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			img, l := ext4test.BuildLayout(r.opts, r.files)
			if !r.raw {
				l.Unlink(img, "/gone", ext4test.DebugfsRm)
			}
			if r.mutate != nil {
				r.mutate(img, l)
			}
			f := openImg(t, img)
			id := staleID(t, f, "gone")
			num, flags, _, err := f.StaleInodeDetail(id)
			switch {
			case r.wantErr != nil:
				if !errors.Is(err, r.wantErr) {
					t.Fatalf("err = %v, want %v", err, r.wantErr)
				}
				if errors.Is(err, filesys.ErrCorrupt) && r.wantErr != filesys.ErrCorrupt {
					t.Errorf("err %v also matches ErrCorrupt", err)
				}
			case r.corrupt:
				var ce *filesys.CorruptError
				if !errors.As(err, &ce) {
					t.Fatalf("err = %v, want a *filesys.CorruptError", err)
				}
			default:
				if err != nil {
					t.Fatalf("err = %v, want the inode accepted", err)
				}
				if num != l.File("/gone").Inode {
					t.Errorf("inode %d, want %d", num, l.File("/gone").Inode)
				}
				if flags&r.flags != r.flags {
					t.Errorf("flags %#x lost %#x", flags, r.flags)
				}
				return
			}
			if r.text != "" && !strings.Contains(err.Error(), r.text) {
				t.Errorf("error %q does not say %q", err, r.text)
			}
		})
	}
}

// An unreadable or bad-checksum inode bitmap is undecided, never "free" (unknown is never absent).
func TestStaleInodeBadBitmapIsUndecided(t *testing.T) {
	files := []ext4test.File{{Path: "/keep", Data: []byte("k")}, {Path: "/gone", Data: []byte("g"), Deleted: true}}
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true, MetadataCsum: true, Groups: 2, InodesPerGroup: 32}, files)
	l.Unlink(img, "/gone", ext4test.DebugfsRm)
	img[l.InodeBitmap[0]*l.BlockSize+3] ^= 0x01 // no checksum refresh
	f := openImg(t, img)
	_, err := f.StaleInode(staleID(t, f, "gone"))
	if !errors.Is(err, filesys.ErrUndecided) {
		t.Fatalf("err = %v, want ErrUndecided", err)
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error %q does not name the checksum", err)
	}
}

// The recovery path reads the inode quietly: a flag note goes to the caller, not to the filesystem's warnings.
func TestStaleInodeAddsNoFilesystemWarning(t *testing.T) {
	files := []ext4test.File{{Path: "/keep", Data: []byte("k")}, {Path: "/gone", Data: []byte("g"), Deleted: true}}
	img, l := ext4test.BuildLayout(ext4test.Options{Groups: 2, InodesPerGroup: 32}, files) // block maps: no extents feature
	l.Unlink(img, "/gone", ext4test.DebugfsRm)
	l.PatchInode(img, l.File("/gone").Inode, orFlags(0x80000)) // the extents flag, without the feature
	f := openImg(t, img)
	id := staleID(t, f, "gone") // listing the entry reads its inode through the live path
	before := append([]string(nil), f.Info().Warnings...)
	_, _, notes, err := f.StaleInodeDetail(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "extents flag") {
		t.Errorf("notes = %q, want one note about the extents flag", notes)
	}
	if after := f.Info().Warnings; strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("warnings changed: before %q after %q", before, after)
	}
	// The live path still warns, as it did before.
	if _, err := f.Inode(l.File("/gone").Inode); err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, w := range f.Info().Warnings {
		warned = warned || strings.Contains(w, "extents flag")
	}
	if !warned {
		t.Error("the live decode no longer warns about the flag")
	}
}
