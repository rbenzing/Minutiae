package f2fs_test

import (
	"bytes"
	"cmp"
	"io"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// TestEveryDirectoryCutCauseLogsItsWarning pins the premise of FuzzF2FSLookup: every cause that makes a directory
// lookup answer with the incomplete error also leaves a warning in Info, so the property cannot go silent when a
// cause stops logging. One row per cause; the lookup is of a name that is absent.
// Mutation: drop any one f.warn of a cut cause; its row fails.
func TestEveryDirectoryCutCauseLogsItsWarning(t *testing.T) {
	rows := []struct {
		name  string
		path  string // the absent name looked up; /d/missing when empty
		limit bool
		warn  string
		build func(t *testing.T) *f2fs.FS
	}{
		{"entry cap", "", true, "more than 3 entries", func(t *testing.T) *f2fs.FS {
			f, _, _ := treeFS(t, treeOpts(), cutDir(5))
			f.SetDirEntryCap(3)
			return f
		}},
		{"damaged block map", "", false, "block map", func(t *testing.T) *f2fs.FS {
			_, img, tree := treeFS(t, treeOpts(), cutDir(5))
			off := int(tree.Addr[tree.NID["/d"]]) * bs
			put32(img, off+360+4*1, 1)
			put32(img, off+16, 3*bs)
			return mustOpen(t, img)
		}},
		{"dentry block of another directory", "/e/missing", false, "shared by directories", func(t *testing.T) *f2fs.FS {
			files := append(cutDir(3), f2fstest.File{Path: "/e", Dir: true}, f2fstest.File{Path: "/e/y", Inline: true})
			_, img, tree := treeFS(t, treeOpts(), files)
			patchDirMap(img, tree, "/e", tree.DirBlocks["/d"][0], 1)
			f := mustOpen(t, img)
			lookupName(t, f, "/d/f001", "f001") // /d claims its block first
			return f
		}},
		{"damaged dentry slots", "", false, "damaged dentry slots", func(t *testing.T) *f2fs.FS {
			_, img, tree := treeFS(t, treeOpts(), cutDir(2))
			blk, area := dotBlock()
			area.Put(2, f2fstest.Dentry{Name: []byte("ok"), Ino: tree.NID["/d/f000"], Type: f2fstest.FTReg})
			area.Put(212, f2fstest.Dentry{Name: []byte("abcdefgh"), Ino: tree.NID["/d/f001"], Type: f2fstest.FTReg, NameLen: 40})
			setDentryBlock(t, img, tree, "/d", 0, blk)
			return mustOpen(t, img)
		}},
		{"unreadable block", "", false, "is unreadable", func(t *testing.T) *f2fs.FS {
			_, img, tree := treeFS(t, treeOpts(), cutDir(250))
			second := int64(tree.DirBlocks["/d"][1]) * bs
			r := &rangeFault{r: bytes.NewReader(img), lo: second, hi: second + bs, err: io.ErrUnexpectedEOF}
			f, err := f2fs.Open(r, int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			r.armed = true
			return f
		}},
		{"size above the read limit", "", false, "64 MiB", func(t *testing.T) *f2fs.FS {
			_, img, tree := treeFS(t, treeOpts(), cutDir(2))
			put32(img, int(tree.Addr[tree.NID["/d"]])*bs+16+4, 1<<20)
			return mustOpen(t, img)
		}},
		{"read budget", "", false, "directory read budget", func(t *testing.T) *f2fs.FS {
			f, _, _ := treeFS(t, treeOpts(), cutDir(450))
			f.SetDirBudget(2 * bs)
			return f
		}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := r.build(t)
			_, err := f.Lookup(cmp.Or(r.path, "/d/missing"))
			wantIncomplete(t, r.name, err, r.limit)
			if !hasWarning(f.Info(), r.warn) {
				t.Errorf("the cut %q left no %q warning: %v", r.name, r.warn, f.Info().Warnings)
			}
		})
	}
}
