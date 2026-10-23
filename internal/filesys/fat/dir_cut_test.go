package fat_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// cutDirImage is a FAT16 volume (one 512-byte sector per cluster, so 16
// entries per directory cluster) whose directory /D holds "." and ".." and n
// files F00.TXT.. (n = 30 fills exactly two clusters with no end marker; n = 20
// leaves the end marker in the second cluster). chain is the cluster chain of
// /D.
func cutDirImage(t *testing.T, n int) (img []byte, g fattest.Geometry, chain []uint32) {
	t.Helper()
	o := fattest.Options{Type: 16}
	g = fattest.Layout(o)
	files := []fattest.File{{Path: "D", Dir: true}}
	for i := range n {
		files = append(files, fattest.File{Path: fmt.Sprintf("D/F%02d.TXT", i), Data: []byte{1}})
	}
	img = build(o, files...)
	f := openImg(t, img)
	d, err := f.Lookup("/D")
	if err != nil {
		t.Fatal(err)
	}
	chain, err = f.Chain(mustCluster(t, d))
	if err != nil || len(chain) != (n+2+15)/16 {
		t.Fatalf("setup: chain %v, %v", chain, err)
	}
	return img, g, chain
}

// setFATs writes entry c of every FAT copy.
func setFATs(img []byte, g fattest.Geometry, c, v uint32) {
	for n := range g.NumFATs {
		setFAT(img, g, n, c, v)
	}
}

// wantIncomplete checks the error of a lookup that could not read its
// directory to the end.
func wantIncomplete(t *testing.T, what string, err error, limit bool) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s: no error", what)
		return
	case !errors.Is(err, filesys.ErrIncomplete):
		t.Errorf("%s: %v does not match filesys.ErrIncomplete", what, err)
	case errors.Is(err, filesys.ErrNotFound):
		t.Errorf("%s: %v matches filesys.ErrNotFound", what, err)
	case !strings.Contains(err.Error(), "cannot be decided"):
		t.Errorf("%s: %q does not say the lookup cannot be decided", what, err)
	}
	if limit != errors.Is(err, filesys.ErrLimit) {
		t.Errorf("%s: errors.Is(err, ErrLimit) = %v, want %v (%v)", what, !limit, limit, err)
	}
	if !limit && !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("%s: %v does not match filesys.ErrCorrupt", what, err)
	}
}

func TestLookupAfterCapIsAnError(t *testing.T) {
	img, _, _ := cutDirImage(t, 6)
	f := openImg(t, img)
	// "." and ".." count as entries: with a cap of 4, F00.TXT and F01.TXT are read.
	f.SetDirEntryCap(4)
	_, err := f.Lookup("/D/F05.TXT")
	wantIncomplete(t, "F05.TXT after the cap", err, true)
	if _, err := f.Lookup("/D/F01.TXT"); err != nil {
		t.Errorf("F01.TXT, read before the cut: %v", err)
	}
}

func TestLookupAtExactlyTheCapIsNotCut(t *testing.T) {
	five := func() []fattest.File {
		var fs []fattest.File
		for i := range 5 {
			fs = append(fs, fattest.File{Path: fmt.Sprintf("F%d.TXT", i), Data: []byte{1}})
		}
		return fs
	}
	f := openImg(t, build(fattest.Options{Type: 16}, five()...))
	f.SetDirEntryCap(5)
	if _, err := f.Lookup("/MISSING.TXT"); !errors.Is(err, filesys.ErrNotFound) || errors.Is(err, filesys.ErrIncomplete) {
		t.Errorf("five entries and an end marker under a cap of 5: %v, want not found", err)
	}
	es, err := f.ReadDir(f.Root())
	if err != nil || len(es) != 5 {
		t.Errorf("ReadDir at exactly the cap = %d entries, %v; want 5 and no error", len(es), err)
	}
	f = openImg(t, build(fattest.Options{Type: 16}, append(five(), fattest.File{Path: "F5.TXT", Data: []byte{1}})...))
	f.SetDirEntryCap(5)
	_, err = f.Lookup("/MISSING.TXT")
	wantIncomplete(t, "six entries under a cap of 5", err, true)
}

func TestLookupInDamagedChainIsAnError(t *testing.T) {
	for _, c := range []struct {
		name   string
		damage func(img []byte, g fattest.Geometry, chain []uint32)
	}{
		{"second cluster free", func(img []byte, g fattest.Geometry, ch []uint32) { setFATs(img, g, ch[1], 0) }},
		{"loop back to the first cluster", func(img []byte, g fattest.Geometry, ch []uint32) { setFATs(img, g, ch[1], ch[0]) }},
		{"first cluster points beyond the volume", func(img []byte, g fattest.Geometry, ch []uint32) { setFATs(img, g, ch[0], 0xF000) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			img, g, chain := cutDirImage(t, 30)
			c.damage(img, g, chain)
			f := openImg(t, img)
			_, err := f.Lookup("/D/MISSING.TXT")
			wantIncomplete(t, "a missing name", err, false)
			if _, err := f.Lookup("/D/F01.TXT"); err != nil {
				t.Errorf("F01.TXT, read before the damage: %v", err)
			}
		})
	}
}

func TestLookupAfterUnreadableClusterIsAnError(t *testing.T) {
	img, g, chain := cutDirImage(t, 30)
	off := g.ClusterOffset(chain[1])
	r := &failingReader{r: bytes.NewReader(img), lo: off, hi: off + int64(g.SectorSize*g.SecPerClus)}
	r.armed.Store(true)
	f, err := fat.Open(r, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Lookup("/D/F29.TXT")
	wantIncomplete(t, "F29.TXT in the unreadable cluster", err, false)
	if _, err := f.Lookup("/D/F01.TXT"); err != nil {
		t.Errorf("F01.TXT, read before the failure: %v", err)
	}
}

func TestLookupAfterBudgetCutIsAnError(t *testing.T) {
	img, _, _ := cutDirImage(t, 30)
	f := openImg(t, img)
	// The root listing takes one 16-entry chunk, the first chunk of /D another;
	// the second chunk of /D finds the budget spent.
	f.SetDirBudget(32)
	_, err := f.Lookup("/D/F29.TXT")
	wantIncomplete(t, "F29.TXT after the budget", err, true)
	if !hasWarning(f, "budget") {
		t.Errorf("no budget warning: %q", f.Info().Warnings)
	}
}

// Pins existing behaviour (mutation: set the cut on any chain error).
func TestLookupDamageAfterEndMarkerIsNotACut(t *testing.T) {
	img, g, chain := cutDirImage(t, 20) // the end marker is entry 22, in the second cluster
	setFATs(img, g, chain[1], 0xF000)   // the chain leaves the volume after the second cluster
	f := openImg(t, img)
	if _, err := f.Lookup("/D/MISSING.TXT"); !errors.Is(err, filesys.ErrNotFound) || errors.Is(err, filesys.ErrIncomplete) {
		t.Errorf("a missing name in a directory read to its end marker: %v, want not found", err)
	}
	d, err := f.Lookup("/D")
	if err != nil {
		t.Fatal(err)
	}
	if es, err := f.ReadDir(d); err != nil || len(es) != 20 {
		t.Errorf("ReadDir = %d entries, %v; want 20 and no error", len(es), err)
	}
}

// Pins existing behaviour (mutation: always set a cut).
func TestLookupWithoutDamageStaysNotFound(t *testing.T) {
	img, _, _ := cutDirImage(t, 20)
	f := openImg(t, img)
	if _, err := f.Lookup("/D/MISSING.TXT"); !errors.Is(err, filesys.ErrNotFound) || errors.Is(err, filesys.ErrIncomplete) {
		t.Errorf("a missing name in an intact directory: %v, want not found", err)
	}
}

func TestReadDirOfCutDirectoryReturnsEntriesAndError(t *testing.T) {
	t.Run("cap", func(t *testing.T) {
		img, _, _ := cutDirImage(t, 6)
		f := openImg(t, img)
		f.SetDirEntryCap(4)
		d, err := f.Lookup("/D")
		if err != nil {
			t.Fatal(err)
		}
		es, err := f.ReadDir(d)
		if got := names(es); !slices.Equal(got, []string{"F00.TXT", "F01.TXT"}) {
			t.Errorf("entries = %v, want the two before the cap", got)
		}
		if !errors.Is(err, filesys.ErrIncomplete) || !errors.Is(err, filesys.ErrLimit) {
			t.Errorf("error = %v, want ErrIncomplete and ErrLimit", err)
		}
	})
	t.Run("damaged chain", func(t *testing.T) {
		img, g, chain := cutDirImage(t, 30)
		setFATs(img, g, chain[0], 0xF000)
		f := openImg(t, img)
		d, err := f.Lookup("/D")
		if err != nil {
			t.Fatal(err)
		}
		es, err := f.ReadDir(d)
		if len(es) != 14 || es[0].Name != "F00.TXT" || es[13].Name != "F13.TXT" {
			t.Errorf("entries = %v, want the 14 files of the first cluster", names(es))
		}
		if !errors.Is(err, filesys.ErrIncomplete) || !errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, filesys.ErrLimit) {
			t.Errorf("error = %v, want ErrIncomplete and ErrCorrupt, not ErrLimit", err)
		}
	})
}

func TestResolveDirBehindCutListingIsNotNotFound(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	files := []fattest.File{{Path: "A", Dir: true}}
	for i := range 14 { // "." + ".." + 14 files fill the first cluster of A; B starts the second
		files = append(files, fattest.File{Path: fmt.Sprintf("A/F%02d.TXT", i), Data: []byte{1}})
	}
	files = append(files, fattest.File{Path: "A/B", Dir: true})
	img := build(o, files...)
	f := openImg(t, img)
	a, err := f.Lookup("/A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Lookup("/A/B")
	if err != nil {
		t.Fatal(err)
	}
	chain, err := f.Chain(mustCluster(t, a))
	if err != nil || len(chain) != 2 {
		t.Fatalf("setup: chain %v, %v", chain, err)
	}
	setFATs(img, g, chain[0], 0xF000) // A is cut before B's entry
	fresh := openImg(t, img)
	_, err = fresh.ReadDir(filesys.Entry{ID: "dir:" + attr(b, "first_cluster")})
	if err == nil || errors.Is(err, filesys.ErrNotFound) || !errors.Is(err, filesys.ErrIncomplete) {
		t.Errorf("ReadDir of a directory behind a cut listing = %v, want an error matching ErrIncomplete and not ErrNotFound", err)
	}
}

// Pins existing behaviour (mutation: skip the subdirectories of a cut listing).
func TestResolveDirDescendsThroughACutListing(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	files := []fattest.File{{Path: "A", Dir: true}, {Path: "A/B", Dir: true}, {Path: "A/B/C", Dir: true}, {Path: "A/B/C/IN.TXT", Data: []byte("in")}}
	for i := range 20 {
		files = append(files, fattest.File{Path: fmt.Sprintf("A/F%02d.TXT", i), Data: []byte{1}})
	}
	img := build(o, files...)
	f := openImg(t, img)
	a, err := f.Lookup("/A")
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.Lookup("/A/B/C")
	if err != nil {
		t.Fatal(err)
	}
	chain, err := f.Chain(mustCluster(t, a))
	if err != nil || len(chain) != 2 {
		t.Fatalf("setup: chain %v, %v", chain, err)
	}
	setFATs(img, g, chain[0], 0xF000) // A is cut after B's entry
	fresh := openImg(t, img)
	es, err := fresh.ReadDir(filesys.Entry{ID: "dir:" + attr(c, "first_cluster")})
	if err != nil || len(es) != 1 || es[0].Name != "IN.TXT" {
		t.Errorf("ReadDir of C through the cut /A = %v, %v", names(es), err)
	}
	if _, err := openImg(t, img).Lookup("/A/B/C"); err != nil {
		t.Errorf("Lookup(/A/B/C): %v", err)
	}
}

// dirOf names the directory whose chain starts with chain[0].
func dirOf(chain []uint32) filesys.Entry {
	return filesys.Entry{ID: fmt.Sprintf("dir:%d", chain[0])}
}

func TestEveryDirectoryCutCauseLogsItsWarning(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T) (*fat.FS, filesys.Entry)
		warn  string
	}{
		{"entry cap", func(t *testing.T) (*fat.FS, filesys.Entry) {
			img, _, chain := cutDirImage(t, 30)
			f := openImg(t, img)
			f.SetDirEntryCap(4)
			return f, dirOf(chain)
		}, "has more than"},
		{"chain bound", func(t *testing.T) (*fat.FS, filesys.Entry) {
			img, _, chain := cutDirImage(t, 30)
			f := openImg(t, img)
			f.SetDirEntryCap(16) // one cluster; the chain has two
			return f, dirOf(chain)
		}, "is longer than"},
		{"damaged chain", func(t *testing.T) (*fat.FS, filesys.Entry) {
			img, g, chain := cutDirImage(t, 30)
			setFATs(img, g, chain[0], 0xF000)
			return openImg(t, img), dirOf(chain)
		}, "is damaged after"},
		{"budget", func(t *testing.T) (*fat.FS, filesys.Entry) {
			img, _, chain := cutDirImage(t, 30)
			f := openImg(t, img)
			f.SetDirBudget(32)
			return f, dirOf(chain)
		}, "budget"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, d := c.setup(t)
			_, err := f.ReadDir(d)
			if !errors.Is(err, filesys.ErrIncomplete) {
				t.Errorf("ReadDir error = %v, want ErrIncomplete", err)
			}
			if !hasWarning(f, c.warn) {
				t.Errorf("no warning containing %q: %q", c.warn, f.Info().Warnings)
			}
		})
	}
}

func TestWalkDescendsIntoACutFATDirectory(t *testing.T) {
	f := openImg(t, build(fattest.Options{Type: 16},
		fattest.File{Path: "SUB", Dir: true}, fattest.File{Path: "SUB/IN.TXT", Data: []byte("in")},
		fattest.File{Path: "A.TXT", Data: []byte{1}}, fattest.File{Path: "B.TXT", Data: []byte{1}}, fattest.File{Path: "C.TXT", Data: []byte{1}}))
	f.SetDirEntryCap(3) // the root is cut after its third entry; SUB (".", "..", IN.TXT) is whole
	var seen, errPaths []string
	err := filesys.Walk(f, f.Root(), "/", func(p string, _ filesys.Entry, err error) error {
		if err != nil {
			if !errors.Is(err, filesys.ErrIncomplete) {
				t.Errorf("%s: %v", p, err)
			}
			errPaths = append(errPaths, p)
			return nil
		}
		seen = append(seen, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(errPaths, []string{"/"}) {
		t.Errorf("the callback received errors for %v, want the root once", errPaths)
	}
	if !slices.Contains(seen, "/SUB/IN.TXT") {
		t.Errorf("the walk did not descend into SUB: %v", seen)
	}
}

func TestLookupCutErrorNamesTheCause(t *testing.T) {
	img, g, chain := cutDirImage(t, 30)
	f := openImg(t, img)
	f.SetDirEntryCap(4)
	_, err := f.Lookup("/D/F29.TXT")
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("cluster %d", chain[0])) || !strings.Contains(err.Error(), "more than 4 entries") {
		t.Errorf("cap error = %v, want the directory cluster %d and the cause", err, chain[0])
	}
	setFATs(img, g, chain[0], 0xF000)
	_, err = openImg(t, img).Lookup("/D/F29.TXT")
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("cluster %d", chain[0])) || !strings.Contains(err.Error(), "outside the") {
		t.Errorf("chain error = %v, want the directory cluster %d and the damaged chain", err, chain[0])
	}
}

// The entry ID of a slot behind a cut is undecidable, never "beyond the end of
// the directory" (pins the broken-chain half; mutation: dirEnd returns the cut
// index).
func TestEntryIDBeyondACutIsNotNotFound(t *testing.T) {
	img, g, chain := cutDirImage(t, 30)
	setFATs(img, g, chain[0], 0xF000)
	f := openImg(t, img)
	d := fmt.Sprint(chain[0])
	if _, err := f.Open(filesys.Entry{ID: "dirent:" + d + ":3"}); err != nil {
		t.Errorf("Open of an entry before the cut: %v", err)
	}
	beyond := filesys.Entry{ID: "dirent:" + d + ":20"}
	if _, err := f.Open(beyond); err == nil || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Open beyond the cut = %v, want an error that is not ErrNotFound", err)
	}
	if _, err := f.ReadDir(beyond); err == nil || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("ReadDir beyond the cut = %v, want an error that is not ErrNotFound", err)
	}
	// The same through the entry cap: the slot is physically there, but the
	// cut index is never the end of the directory.
	img, _, _ = cutDirImage(t, 30)
	f = openImg(t, img)
	f.SetDirEntryCap(4)
	if _, err := f.Open(filesys.Entry{ID: "dirent:" + d + ":10"}); errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Open of entry 10 under a cap of 4 = %v, want not ErrNotFound", err)
	}
}

// Pins existing behaviour (mutation: wrap ErrIncomplete into the budget error).
func TestBudgetSpentBeforeFirstEntryStaysCorruptOnly(t *testing.T) {
	f := openImg(t, build(fattest.Options{Type: 16}, fattest.File{Path: "A.TXT", Data: []byte{1}}))
	f.SetDirBudget(0)
	es, err := f.ReadDir(f.Root())
	var ce *filesys.CorruptError
	if len(es) != 0 || !errors.As(err, &ce) || !strings.Contains(err.Error(), "directory read budget exhausted") {
		t.Fatalf("ReadDir = %d entries, %v; want a CorruptError \"directory read budget exhausted\"", len(es), err)
	}
	if errors.Is(err, filesys.ErrIncomplete) {
		t.Errorf("a directory that could not be started matches ErrIncomplete: %v", err)
	}
}
