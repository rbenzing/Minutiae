package exfat_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

// dirWithFiles is /dir (a FatChain directory) holding n empty files f000...
func dirWithFiles(n int) []exfattest.File {
	files := []exfattest.File{{Path: "/dir", Dir: true, FatChain: true}}
	for i := range n {
		files = append(files, exfattest.File{Path: fmt.Sprintf("/dir/f%03d", i)})
	}
	return files
}

// loopDirImage is an image whose /dir has three clusters (120 files) and whose
// chain loops back after the second one: the third cluster, and with it the
// end-of-directory marker, is never reached.
func loopDirImage(t *testing.T) []byte {
	t.Helper()
	img, lay := exfattest.BuildLayout(exfattest.Options{}, dirWithFiles(120))
	dc := lay.Find("/dir", false).Clusters
	if len(dc) != 3 {
		t.Fatalf("directory has %d clusters, want 3", len(dc))
	}
	le32(img, int(lay.FatEntryOffset(dc[1])), dc[0])
	return img
}

func TestExfatLookupInDamagedChainIsAnError(t *testing.T) {
	f := openImg(t, loopDirImage(t))
	if err := lookupErr(f, "/dir/f000"); err != nil { // in the first cluster
		t.Errorf("a name before the damage: %v", err)
	}
	err := lookupErr(f, "/dir/f119") // in the third cluster
	if !errors.Is(err, filesys.ErrIncomplete) || !errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup beyond the damage = %v, want ErrIncomplete and ErrCorrupt", err)
	}
}

// Pins existing behaviour: damage after the end marker is not a cut (mutation:
// set the cut for any partial chain).
func TestExfatLookupDamageAfterEndMarkerIsNotACut(t *testing.T) {
	img, lay := exfattest.BuildLayout(exfattest.Options{}, dirWithFiles(10))
	loc := lay.Find("/dir", false)
	if len(loc.Clusters) != 1 {
		t.Fatalf("directory has %d clusters, want 1", len(loc.Clusters))
	}
	le64(img, int(loc.Offset)+32+24, 2*4096) // DataLength: two clusters, the second is not chained
	le64(img, int(loc.Offset)+32+8, 2*4096)
	exfattest.FixSetChecksum(img, loc.Offset)
	f := openImg(t, img)
	if err := lookupErr(f, "/dir/f009"); err != nil {
		t.Errorf("Lookup(/dir/f009): %v", err)
	}
	if err := lookupErr(f, "/dir/zzz"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(/dir/zzz) = %v, want ErrNotFound (the end marker was seen)", err)
	}
	d, err := f.Lookup("/dir")
	if err != nil {
		t.Fatal(err)
	}
	if es, err := f.ReadDir(d); err != nil || len(es) != 10 {
		t.Errorf("ReadDir = %d entries, %v; want 10 and nil", len(es), err)
	}
	if !hasWarn(f, "only the part before the damage") {
		t.Errorf("the damaged chain left no warning: %v", f.Info().Warnings)
	}
}

func TestExfatLookupInOversizeDirectoryIsAnError(t *testing.T) {
	img := exfattest.Build(exfattest.Options{}, dirWithFiles(70)) // 211 entries: two clusters
	f := openImg(t, img)
	f.SetDirByteLimit(4096)
	if err := lookupErr(f, "/dir/zzz"); !errors.Is(err, filesys.ErrIncomplete) || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup with the limit below the directory = %v, want ErrIncomplete", err)
	}
	g := openImg(t, img)
	g.SetDirByteLimit(8192)
	if err := lookupErr(g, "/dir/zzz"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup with the limit at the directory size = %v, want ErrNotFound", err)
	}
}

// RED today: ReadDir returns nil for a directory whose chain is damaged.
func TestExfatReadDirOfDamagedChainReturnsEntriesAndError(t *testing.T) {
	f := openImg(t, loopDirImage(t))
	d, err := f.Lookup("/dir")
	if err != nil {
		t.Fatal(err)
	}
	es, err := f.ReadDir(d)
	if len(es) == 0 || len(es) >= 120 {
		t.Errorf("ReadDir listed %d entries, want those of the two clusters before the loop", len(es))
	}
	if !errors.Is(err, filesys.ErrIncomplete) || !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("ReadDir error = %v, want ErrIncomplete and ErrCorrupt", err)
	}
}

// dirCutCase is one way a directory scan is cut: set up an instance whose
// lookup of a missing name meets the cut.
type dirCutCase struct {
	name        string
	setup       func(t *testing.T) (f *exfat.FS, path string)
	warn        string // the warning the cause leaves in Info
	wantLimit   bool   // ErrLimit is the cause
	wantCorrupt bool   // ErrCorrupt is the cause
}

func dirCutCases() []dirCutCase {
	return []dirCutCase{
		{"entry cap", func(t *testing.T) (*exfat.FS, string) {
			files := []exfattest.File{{Path: "/a"}, {Path: "/b"}, {Path: "/c"}}
			f := openImg(t, exfattest.Build(exfattest.Options{}, files))
			f.SetDirRecordCap(1)
			return f, "/zzz"
		}, "more than", true, false},
		{"read budget", func(t *testing.T) (*exfat.FS, string) {
			f := openImg(t, exfattest.Build(exfattest.Options{}, dirWithFiles(70)))
			f.SetDirBudget(8192) // the root and the first cluster of /dir
			return f, "/dir/zzz"
		}, "budget", true, false},
		{"unreadable chunk", func(t *testing.T) (*exfat.FS, string) {
			f, _, _ := cutDirImage(t)
			return f, "/D1/zzz"
		}, "unreadable", false, false},
		{"damaged chain", func(t *testing.T) (*exfat.FS, string) {
			return openImg(t, loopDirImage(t)), "/dir/zzz"
		}, "only the part before the damage", false, true},
		{"byte limit", func(t *testing.T) (*exfat.FS, string) {
			f := openImg(t, exfattest.Build(exfattest.Options{}, dirWithFiles(70)))
			f.SetDirByteLimit(4096)
			return f, "/dir/zzz"
		}, "limit", false, true},
	}
}

// RED today: the message says "not found in an incomplete directory listing".
func TestExfatLookupCutErrorSaysCannotBeDecided(t *testing.T) {
	for _, c := range dirCutCases() {
		t.Run(c.name, func(t *testing.T) {
			f, path := c.setup(t)
			err := lookupErr(f, path)
			if err == nil || !errors.Is(err, filesys.ErrIncomplete) {
				t.Fatalf("Lookup(%q) = %v, want ErrIncomplete", path, err)
			}
			if errors.Is(err, filesys.ErrLimit) != c.wantLimit {
				t.Errorf("errors.Is(ErrLimit) = %v, want %v (%v)", !c.wantLimit, c.wantLimit, err)
			}
			if c.wantCorrupt && !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("error %v does not wrap ErrCorrupt", err)
			}
			if errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("error %v is also ErrNotFound", err)
			}
			if !strings.Contains(err.Error(), "cannot be decided") || strings.Contains(err.Error(), "not found") {
				t.Errorf("error %q must say the lookup cannot be decided and never not found", err)
			}
		})
	}
}

// The exFAT twin of the ext4 and F2FS test: every cut cause leaves its warning.
func TestEveryDirectoryCutCauseLogsItsWarning(t *testing.T) {
	for _, c := range dirCutCases() {
		t.Run(c.name, func(t *testing.T) {
			f, path := c.setup(t)
			if err := lookupErr(f, path); err == nil {
				t.Fatal("the lookup in a cut directory succeeded")
			}
			if !hasWarn(f, c.warn) {
				t.Errorf("no warning containing %q: %v", c.warn, f.Info().Warnings)
			}
		})
	}
}

// Found by the fuzz property (N20): a damaged File set that is a directory was
// listed with a dir: ID that no claim backs, so a lookup in it was ErrNotFound.
func TestExfatLookupInDamagedDirectorySetIsNotAbsent(t *testing.T) {
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/d", Dir: true}, {Path: "/d/x", Data: []byte("x")}})
	img[lay.Find("/d", false).Offset+1] = 191 // SecondaryCount: a File set has at most 18
	f := openImg(t, img)
	d, err := f.Lookup("/d")
	if err != nil {
		t.Fatal(err)
	}
	if !hasAttr(d, "set", "damaged") {
		t.Fatalf("/d attrs = %v, want set=damaged", d.Attrs)
	}
	if _, err := f.ReadDir(d); !errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("ReadDir of a damaged directory set = %v, want ErrCorrupt", err)
	}
	if err := lookupErr(f, "/d/missing"); !errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup in a damaged directory set = %v, want ErrCorrupt, never ErrNotFound", err)
	}
}
