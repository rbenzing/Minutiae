package f2fs_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// wantIncomplete checks the typed incomplete-scan error: it wraps filesys.ErrIncomplete, is ErrLimit for the
// entry cap and ErrCorrupt for the other causes, and is never "not found", ambiguous or undecided.
func wantIncomplete(t *testing.T, what string, err error, limit bool) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s: no error", what)
	case !errors.Is(err, filesys.ErrIncomplete):
		t.Errorf("%s: %v does not wrap ErrIncomplete", what, err)
	case limit && !errors.Is(err, filesys.ErrLimit), !limit && !errors.Is(err, filesys.ErrCorrupt):
		t.Errorf("%s: %v has the wrong kind (limit %v)", what, err, limit)
	case errors.Is(err, filesys.ErrNotFound), errors.Is(err, filesys.ErrAmbiguous), errors.Is(err, filesys.ErrUndecided):
		t.Errorf("%s: %v is also not-found, ambiguous or undecided", what, err)
	}
}

// cutDir is /d holding n inline files f000..; a directory of 212 or fewer entries has one dentry block (the first
// block also holds "." and ".."), more has two.
func cutDir(n int) []f2fstest.File {
	files := []f2fstest.File{{Path: "/d", Dir: true}}
	for i := range n {
		files = append(files, f2fstest.File{Path: fmt.Sprintf("/d/f%03d", i), Inline: true})
	}
	return files
}

func lookupName(t *testing.T, f *f2fs.FS, p, want string) {
	t.Helper()
	if e, err := f.Lookup(p); err != nil || e.Name != want {
		t.Errorf("Lookup(%s) = %+v, %v; want %q", p, e, err, want)
	}
}

func TestLookupAfterCapIsAnError(t *testing.T) {
	f, _, _ := treeFS(t, treeOpts(), cutDir(5))
	f.SetDirEntryCap(3)
	_, err := f.Lookup("/d/f004")
	wantIncomplete(t, "an entry beyond the cap", err, true)
	_, err = f.Lookup("/d/missing")
	wantIncomplete(t, "a missing name after the cap", err, true)
	lookupName(t, f, "/d/f000", "f000")
	lookupName(t, f, "/d/f002", "f002")
}

// Exactly at the cap is not over it (pins existing behaviour); one more entry is. Mutation: set the cut when the
// count REACHES the cap instead of when a record beyond it is seen.
func TestLookupAtExactlyTheCapIsNotCut(t *testing.T) {
	f, _, _ := treeFS(t, treeOpts(), cutDir(5))
	f.SetDirEntryCap(5)
	if _, err := f.Lookup("/d/missing"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a missing name in a directory of exactly cap entries = %v, want not found", err)
	}
	g, _, _ := treeFS(t, treeOpts(), cutDir(6))
	g.SetDirEntryCap(5)
	_, err := g.Lookup("/d/missing")
	wantIncomplete(t, "cap+1 entries", err, true)
}

func TestLookupInDamagedBlockMapIsAnError(t *testing.T) {
	_, img, tree := treeFS(t, treeOpts(), cutDir(5))
	off := int(tree.Addr[tree.NID["/d"]]) * bs
	put32(img, off+360+4*1, 1) // block 1 maps below the main area
	put32(img, off+16, 3*bs)
	f := mustOpen(t, img)
	lookupName(t, f, "/d/f000", "f000")
	_, err := f.Lookup("/d/missing")
	wantIncomplete(t, "a missing name after a damaged block map", err, false)
	if !hasWarning(f.Info(), "block map") {
		t.Errorf("no block map warning: %v", f.Info().Warnings)
	}
}

// A block claimed by another directory, or mapped twice by the same one, is skipped with a warning, so the
// names it holds are not in view: a lookup that finds nothing says so.
func TestLookupSkipsSharedDentryBlockAndSaysSo(t *testing.T) {
	t.Run("claimed by another directory", func(t *testing.T) {
		files := append(cutDir(3), f2fstest.File{Path: "/e", Dir: true}, f2fstest.File{Path: "/e/y", Inline: true})
		_, img, tree := treeFS(t, treeOpts(), files)
		patchDirMap(img, tree, "/e", tree.DirBlocks["/d"][0], 1) // /e now maps /d's block
		f := mustOpen(t, img)
		lookupName(t, f, "/d/f001", "f001") // /d claims its block first
		_, err := f.Lookup("/e/y")
		wantIncomplete(t, "a name of the directory whose block another directory owns", err, false)
		if !hasWarning(f.Info(), "shared by directories") {
			t.Errorf("no sharing warning: %v", f.Info().Warnings)
		}
	})
	t.Run("mapped twice by one scan", func(t *testing.T) {
		_, img, tree := treeFS(t, treeOpts(), cutDir(250))
		blocks := tree.DirBlocks["/d"]
		if len(blocks) != 2 {
			t.Fatalf("/d has %d dentry blocks, want 2", len(blocks))
		}
		off := int(tree.Addr[tree.NID["/d"]]) * bs
		put32(img, off+360+4*1, blocks[0]) // block 1 maps block 0 again
		f := mustOpen(t, img)
		lookupName(t, f, "/d/f000", "f000")
		_, err := f.Lookup("/d/f249")
		wantIncomplete(t, "a name of the block that is no longer mapped", err, false)
		if !hasWarning(f.Info(), "shared by directories") {
			t.Errorf("no sharing warning: %v", f.Info().Warnings)
		}
	})
}

func TestLookupWithDamagedDentrySlotsIsAnError(t *testing.T) {
	_, img, tree := treeFS(t, treeOpts(), cutDir(2))
	blk, area := dotBlock()
	area.Put(2, f2fstest.Dentry{Name: []byte("ok"), Ino: tree.NID["/d/f000"], Type: f2fstest.FTReg})
	area.Put(212, f2fstest.Dentry{Name: []byte("abcdefgh"), Ino: tree.NID["/d/f001"], Type: f2fstest.FTReg, NameLen: 40})
	setDentryBlock(t, img, tree, "/d", 0, blk)
	f := mustOpen(t, img)
	lookupName(t, f, "/d/ok", "ok")
	_, err := f.Lookup("/d/missing")
	wantIncomplete(t, "a missing name beside damaged dentry slots", err, false)
	if !hasWarning(f.Info(), "damaged dentry slots") {
		t.Errorf("no damaged-slot warning: %v", f.Info().Warnings)
	}
}

func TestLookupInOversizeDirectoryIsAnError(t *testing.T) {
	_, img, tree := treeFS(t, treeOpts(), cutDir(2))
	put32(img, int(tree.Addr[tree.NID["/d"]])*bs+16+4, 1<<20) // i_size ~ 2^52
	f := mustOpen(t, img)
	lookupName(t, f, "/d/f001", "f001")
	_, err := f.Lookup("/d/missing")
	wantIncomplete(t, "a missing name in an oversize directory", err, false)
	if !hasWarning(f.Info(), "64 MiB") {
		t.Errorf("no 64 MiB warning: %v", f.Info().Warnings)
	}
}

// A read that fails without being an I/O error (a short read) is a cut; the lookup says so.
func TestLookupAfterUnreadableBlockIsAnError(t *testing.T) {
	_, img, tree := treeFS(t, treeOpts(), cutDir(250))
	second := int64(tree.DirBlocks["/d"][1]) * bs
	r := &rangeFault{r: bytes.NewReader(img), lo: second, hi: second + bs, err: io.ErrUnexpectedEOF}
	f, err := f2fs.Open(r, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	r.armed = true
	lookupName(t, f, "/d/f000", "f000")
	for _, n := range []string{"f249", "missing"} {
		_, err := f.Lookup("/d/" + n)
		wantIncomplete(t, "Lookup("+n+") with block 1 unreadable", err, false)
	}
	if !hasWarning(f.Info(), "is unreadable") {
		t.Errorf("no unreadable-block warning: %v", f.Info().Warnings)
	}
}

// A genuine I/O error stays an I/O error (pins existing behaviour). Mutation: route it to the cut path.
func TestLookupAfterIOErrorIsAnIOError(t *testing.T) {
	_, img, tree := treeFS(t, treeOpts(), cutDir(250))
	second := int64(tree.DirBlocks["/d"][1]) * bs
	injected := errors.New("device unplugged")
	r := &rangeFault{r: bytes.NewReader(img), lo: second, hi: second + bs, err: injected}
	f, err := f2fs.Open(r, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	r.armed = true
	for _, n := range []string{"f249", "missing"} {
		_, err := f.Lookup("/d/" + n)
		if !errors.Is(err, injected) || errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, filesys.ErrNotFound) || errors.Is(err, filesys.ErrIncomplete) {
			t.Errorf("Lookup(%s) = %v; want the injected I/O error, not corruption, not found or incomplete", n, err)
		}
	}
}

// ONE filesystem: the cut is a property of one scan, not remembered. Mutation: store it on the FS.
func TestLookupErrorIsNotCached(t *testing.T) {
	f, _, _ := treeFS(t, treeOpts(), cutDir(5))
	f.SetDirEntryCap(3)
	_, err := f.Lookup("/d/f004")
	wantIncomplete(t, "beyond the cap", err, true)
	f.SetDirEntryCap(10)
	lookupName(t, f, "/d/f004", "f004")
	if _, err := f.Lookup("/d/nowhere"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a clean miss after raising the cap = %v, want not found", err)
	}
}

// Pins existing behaviour. Mutation: always set a cut.
func TestLookupWithoutDamageStaysNotFound(t *testing.T) {
	f, _, _ := treeFS(t, treeOpts(), cutDir(250))
	if _, err := f.Lookup("/d/nowhere"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a missing name in an intact two-block directory = %v, want not found", err)
	}
	if _, err := f.Lookup("/nowhere"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a missing name in the root = %v, want not found", err)
	}
}

// The read budget keeps its existing error: a CorruptError naming the budget (pins existing behaviour; it also
// matches ErrIncomplete now). Mutation: return "no such entry" when the cut came from the read budget.
func TestLookupBudgetCutKeepsItsError(t *testing.T) {
	f, _, _ := treeFS(t, treeOpts(), cutDir(450))
	for _, p := range []string{"/d/f449", "/d/no-such-name"} {
		f.SetDirBudget(2 * bs) // the root's block and the first block of /d
		_, err := f.Lookup(p)
		var ce *filesys.CorruptError
		if errors.Is(err, filesys.ErrNotFound) || !errors.As(err, &ce) || !strings.Contains(ce.Reason, "directory read budget exhausted") {
			t.Errorf("Lookup(%q) = %v; want a CorruptError about the read budget", p, err)
		}
		wantIncomplete(t, "Lookup("+p+") past the budget", err, false)
	}
	f.SetDirBudget(2 * bs)
	lookupName(t, f, "/d/f000", "f000")
}

func cfFiles(names ...string) []f2fstest.File {
	files := []f2fstest.File{{Path: "/cf", Dir: true, Casefold: true}}
	for _, n := range names {
		files = append(files, f2fstest.File{Path: "/cf/" + n, Inline: true})
	}
	return files
}

func cfTree(t *testing.T, names ...string) (*f2fs.FS, []byte, *f2fstest.Tree) {
	t.Helper()
	return treeFS(t, f2fstest.Options{Segments: 2, Encoding: 1}, cfFiles(names...))
}

// Ruling Z18: a fold decision is made only from a complete scan. The cap cuts the scan after exactly one fold
// hit and before the exact name: the answer is the incomplete error, not the fold hit and not not-found.
func TestCasefoldIncompleteScanBeatsFoldDecision(t *testing.T) {
	f, _, _ := cfTree(t, "Stra"+sharpS+"e", "pad1", "pad2", "STRASSE")
	f.SetDirEntryCap(3)
	_, err := f.Lookup("/cf/STRASSE")
	wantIncomplete(t, "the exact name beyond the cap with one fold hit before it", err, true)
	// a second fold hit beyond the cut would make it ambiguous: still the incomplete error
	g, _, _ := cfTree(t, "Stra"+sharpS+"e", "pad1", "pad2", "strasse")
	g.SetDirEntryCap(3)
	_, err = g.Lookup("/cf/STRASSE")
	wantIncomplete(t, "a possible second hit beyond the cap", err, true)
	// an undecidable name before the cut does not turn the incomplete error into an undecided one
	h, _, _ := cfTree(t, "x"+string([]byte{0xFF})+"y", "pad1", "pad2", "z")
	h.SetDirEntryCap(3)
	_, err = h.Lookup("/cf/missing")
	wantIncomplete(t, "an undecidable name before the cut", err, true)
	// no cut: the same shapes decide as before
	k, _, _ := cfTree(t, "Stra"+sharpS+"e")
	if e, err := k.Lookup("/cf/STRASSE"); err != nil || e.Name != "Stra"+sharpS+"e" {
		t.Errorf("a complete scan with one fold hit = %+v, %v", e, err)
	}
}

// The same order for the cuts other than the cap: a damaged dentry slot, a damaged block map and the read
// budget each come after one fold hit and before the end of the directory.
func TestCasefoldEveryCutBeatsFoldDecision(t *testing.T) {
	hit := "Stra" + sharpS + "e"
	t.Run("damaged dentry slot", func(t *testing.T) {
		_, img, tree := cfTree(t, hit, "pad")
		blk, area := dotBlock()
		area.Put(2, f2fstest.Dentry{Name: []byte(hit), Ino: tree.NID["/cf/"+hit], Type: f2fstest.FTReg})
		area.Put(212, f2fstest.Dentry{Name: []byte("abcdefgh"), Ino: tree.NID["/cf/pad"], Type: f2fstest.FTReg, NameLen: 40})
		setDentryBlock(t, img, tree, "/cf", 0, blk)
		f := mustOpen(t, img)
		_, err := f.Lookup("/cf/strasse")
		wantIncomplete(t, "one fold hit beside a damaged slot", err, false)
	})
	t.Run("damaged block map", func(t *testing.T) {
		_, img, tree := cfTree(t, hit, "pad")
		off := int(tree.Addr[tree.NID["/cf"]]) * bs
		put32(img, off+360+4*1, 1)
		put32(img, off+16, 3*bs)
		f := mustOpen(t, img)
		_, err := f.Lookup("/cf/strasse")
		wantIncomplete(t, "one fold hit before a damaged block map", err, false)
	})
	t.Run("read budget", func(t *testing.T) {
		names := []string{hit}
		for i := range 300 {
			names = append(names, fmt.Sprintf("fill%03d", i))
		}
		f, _, _ := cfTree(t, names...)
		f.SetDirBudget(2 * bs) // the root's block and the first block of /cf
		_, err := f.Lookup("/cf/strasse")
		wantIncomplete(t, "one fold hit before the read budget ran out", err, false)
		var ce *filesys.CorruptError
		if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "directory read budget exhausted") {
			t.Errorf("error %v is not the read budget error", err)
		}
	})
}

// An exact (or alias) name that comes BEFORE the cut stands (the scan ends at the exact hit).
func TestCasefoldExactBeforeCutStillWins(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	files := cfFiles("STRASSE", "p1", "p2", "p3", "p4")
	img, _ := f2fstest.BuildTree(f2fstest.Options{Segments: 2, Encoding: 1}, files)
	f := mustOpen(t, img)
	f.SetDirEntryCap(3)
	if e, err := f.Lookup("/cf/STRASSE"); err != nil || e.Name != "STRASSE" {
		t.Errorf("the exact name before the cut = %+v, %v", e, err)
	}
	if _, err := f.Lookup("/cf/p4"); err == nil {
		t.Error("a name beyond the cap resolved")
	}
	// the alias name is the third record, still within the cap
	files = cfFiles("STRASSE", "p1")
	files = append(files, f2fstest.File{Path: "/cf/bad", RawName: []byte(bad), Inline: true}, f2fstest.File{Path: "/cf/p2", Inline: true})
	img, _ = f2fstest.BuildTree(f2fstest.Options{Segments: 2, Encoding: 1}, files)
	g := mustOpen(t, img)
	g.SetDirEntryCap(3)
	if e, err := g.Lookup("/cf/" + alias); err != nil || string(rawName(e)) != bad {
		t.Errorf("the alias name before the cut = %+v, %v", e, err)
	}
}

// Ruling Z20: an exact (or alias) name that lies AFTER a cut is still returned once the scan reaches it: the cut
// is BEFORE the match (block 0 has a damaged slot, the name is in block 1). Mutation: decide the cut before the
// exact/alias decision; this test must fail.
func TestCasefoldExactAndAliasAfterTheCutStillWin(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	for _, c := range []struct{ name, query, want string }{
		{"exact", "Exact.TXT", "Exact.TXT"},
		{"alias", alias, bad},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := cfFiles()
			for i := range 230 {
				files = append(files, f2fstest.File{Path: fmt.Sprintf("/cf/fill%03d", i), Inline: true})
			}
			files = append(files, f2fstest.File{Path: "/cf/target", RawName: []byte(c.want), Inline: true})
			img, tree := f2fstest.BuildTree(f2fstest.Options{Segments: 2, Encoding: 1}, files)
			if n := len(tree.DirBlocks["/cf"]); n != 2 {
				t.Fatalf("/cf has %d dentry blocks, want 2", n)
			}
			blk, area := dotBlock()
			area.Put(212, f2fstest.Dentry{Name: []byte("abcdefgh"), Ino: tree.NID["/cf/fill000"], Type: f2fstest.FTReg, NameLen: 40})
			setDentryBlock(t, img, tree, "/cf", 0, blk) // block 0 is cut by a damaged slot
			f := mustOpen(t, img)
			e, err := f.Lookup("/cf/" + c.query)
			if err != nil || !bytes.Equal(rawName(e), []byte(c.want)) {
				t.Fatalf("Lookup(%q) = %+v, %v; want %q found after the cut", c.query, e, err, c.want)
			}
			// ... while a name that is not there is in doubt, not absent
			_, err = f.Lookup("/cf/nowhere")
			wantIncomplete(t, "a missing name beside the cut", err, false)
		})
	}
}

func TestLookupCutErrorNamesTheCause(t *testing.T) {
	f, _, _ := treeFS(t, treeOpts(), cutDir(5))
	f.SetDirEntryCap(3)
	_, err := f.Lookup("/d/missing")
	if err == nil || !strings.Contains(err.Error(), "cannot be decided") || !strings.Contains(err.Error(), "more than 3 entries") {
		t.Errorf("error %v does not say why the lookup cannot be decided", err)
	}
}
