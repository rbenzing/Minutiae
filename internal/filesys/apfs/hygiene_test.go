package apfs_test

import (
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

func TestInfoWarningsAccumulateAcrossReads(t *testing.T) {
	v := dataVolume(apfstest.File{Path: "/ok", Data: []byte("1")}, apfstest.File{Path: "/ghost", NoInode: true})
	v.Extra = []apfstest.FSRecord{extraDrec(2, hashedTail(1, "\x00"), 16)}
	f, _ := openOpts(t, volOpts(v))
	before := f.Info().Warnings
	if len(before) != 0 {
		t.Fatalf("warnings after Open: %q", before)
	}
	vol := mustLookup(t, f, "/Data")
	if len(f.Info().Warnings) != 0 {
		t.Fatalf("a clean Lookup warned: %q", f.Info().Warnings)
	}
	mustReadDir(t, f, vol)
	after := f.Info().Warnings
	if len(after) != 2 || !hasWarn(f, "empty name") || !hasWarn(f, "whose inode cannot be read") {
		t.Fatalf("warnings after ReadDir: %q", after)
	}
	// The same problems are recorded once, however often they are met.
	mustReadDir(t, f, vol)
	mustReadDir(t, f, vol)
	if got := f.Info().Warnings; !slices.Equal(got, after) {
		t.Errorf("warnings changed on a repeat: %q", got)
	}
	// A snapshot taken earlier is not changed by later warnings.
	if len(before) != 0 {
		t.Error("an earlier Info slice grew")
	}
	f.Warn("later problem")
	if got := f.Info().Warnings; len(got) != 3 || got[2] != "later problem" {
		t.Errorf("warnings: %q", got)
	}
}

// Damage to any block of a volume's objects is a warning or an error, never a
// panic or a runaway: every block is flipped in turn (and re-sealed half of the
// time, so that the damage gets past the checksum) and the whole tree walked.
func TestVolumeDamageNeverPanics(t *testing.T) {
	v := dataVolume(richFiles()...)
	v.TreeMaxKeys = 3
	v.Files = append(v.Files, apfstest.File{Path: "/x/y", Xattrs: []apfstest.Xattr{{Name: "k", Value: []byte("v")}}})
	im := newImage(t, volOpts(v, apfstest.Volume{Name: "Enc", Encrypted: true}))
	vg := im.g.Volumes[0]
	rng := rand.New(rand.NewPCG(7, 9)) //nolint:gosec // deterministic test input, not security
	orig := slices.Clone(im.b)
	walk := func(f *apfs.FS) {
		n := 0
		_ = filesys.Walk(f, f.Root(), "/", func(_ string, _ filesys.Entry, _ error) error {
			if n++; n > 500 {
				return filesys.SkipDir
			}
			return nil
		})
		_, _ = f.Lookup("/Data/docs/a.txt")
		_, _ = f.Lookup("/Data/x/y")
		_ = f.Info()
	}
	for blk := vg.Super; blk < vg.End; blk++ {
		for range 6 {
			copy(im.b, orig)
			b := im.blk(blk)
			for range 1 + rng.IntN(3) {
				b[rng.IntN(len(b))] = byte(rng.IntN(256))
			}
			if rng.IntN(2) == 0 {
				im.seal(blk)
			}
			f, err := im.open()
			if err != nil {
				continue
			}
			walk(f)
		}
	}
}

func TestReadDirIsConcurrentSafe(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(richFiles()...), dataVolume()))
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if es, err := f.ReadDir(filesys.Entry{ID: "n:0:0:2"}); err != nil || len(es) != 3 {
					t.Errorf("goroutine %d: ReadDir = %d entries, %v", g, len(es), err)
					return
				}
				if _, err := f.Lookup("/Data/docs/b.txt"); err != nil {
					t.Errorf("goroutine %d: %v", g, err)
					return
				}
				_ = f.Info()
			}
		}()
	}
	wg.Wait()
}

// A name the lint-free way: the warning text of every skipped record quotes the
// on-disk bytes.
func TestWarningsQuoteOnDiskNames(t *testing.T) {
	v := dataVolume()
	v.Extra = []apfstest.FSRecord{extraDrec(2, hashedTail(5, "a\x00\n\x1b\x00"), 16)}
	f, _ := openOpts(t, volOpts(v))
	mustReadDir(t, f, mustLookup(t, f, "/Data"))
	for _, w := range f.Info().Warnings {
		if strings.ContainsAny(w, "\x00\n\x1b") {
			t.Errorf("warning carries raw bytes: %q", w)
		}
	}
}
