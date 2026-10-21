package ext4_test

import (
	"bytes"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// TestEveryDirectoryCutCauseLogsItsWarning pins the premise of the lookup fuzz properties: every cause that makes a
// directory lookup answer with the incomplete error also leaves a warning in Info, so a property that keys on the
// warning cannot go silent when a cause stops logging. One row per cause; the lookup is of a name that is absent.
// Mutation: drop any one f.warn / w.f.warn of a cut cause; its row fails.
func TestEveryDirectoryCutCauseLogsItsWarning(t *testing.T) {
	rows := []struct {
		name  string
		limit bool
		warn  string
		build func(t *testing.T) *ext4.FS
	}{
		{"record cap", true, "more than 3 entries", func(t *testing.T) *ext4.FS {
			f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "b", "c", "d", "e")))
			f.SetDirRecordCap(3)
			return f
		}},
		{"invalid rec_len", false, "invalid rec_len", func(t *testing.T) *ext4.FS {
			img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "|", "b"))
			blk, _ := dirBlocks(t, mustOpen(t, img), "/d")
			put16(img, blk+1024+4, 0)
			return mustOpen(t, img)
		}},
		{"name_len does not fit", false, "does not fit rec_len", func(t *testing.T) *ext4.FS {
			h := newDirImage(t, true)
			h.img[h.dirent[2]+6] = 255
			return mustOpen(t, h.img)
		}},
		{"tail too short for a record", false, "too short for a record", func(t *testing.T) *ext4.FS {
			h := newDirImage(t, true)
			put16(h.img, h.dirent[4]+4, 1024-72-4) // the last record stops 4 bytes short of the block end
			return mustOpen(t, h.img)
		}},
		{"damaged block map", false, "block map is damaged", func(t *testing.T) *ext4.FS {
			img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "|", "b"))
			ib := inodeOff(t, mustOpen(t, img), 1024, 256, 128, int(ext4test.InodeNumber(0))) + 0x28
			put16(img, ib+12+4, 1)
			put16(img, ib+2, 2)
			put32(img, ib+24, 1)
			put16(img, ib+24+4, 1)
			put16(img, ib+24+6, 0)
			put32(img, ib+24+8, 0xFFFFF0)
			return mustOpen(t, img)
		}},
		{"sparse hole", false, "sparse directory", func(t *testing.T) *ext4.FS {
			img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "|", "b", "|", "c"))
			f := mustOpen(t, img)
			blk, _ := dirBlocks(t, f, "/d")
			ib := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0))) + 0x28
			start := uint32(blk / 1024)
			put16(img, ib+2, 2)
			put32(img, ib+12, 0)
			put16(img, ib+12+4, 1)
			put16(img, ib+12+6, 0)
			put32(img, ib+12+8, start)
			put32(img, ib+24, 2)
			put16(img, ib+24+4, 1)
			put16(img, ib+24+6, 0)
			put32(img, ib+24+8, start+2)
			return mustOpen(t, img)
		}},
		{"unreadable block", false, "is unreadable", func(t *testing.T) *ext4.FS {
			img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "|", "b"))
			blk, _ := dirBlocks(t, mustOpen(t, img), "/d")
			f, err := ext4.Open(failingReader{bytes.NewReader(img), int64(blk) + 1024, int64(blk) + 2048}, int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			return f
		}},
		{"i_size above the read limit", false, "64 MiB", func(t *testing.T) *ext4.FS {
			img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "b"))
			put32(img, inodeOff(t, mustOpen(t, img), 1024, 256, 128, int(ext4test.InodeNumber(0)))+4, 1<<27)
			return mustOpen(t, img)
		}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := r.build(t)
			_, err := f.Lookup("/d/missing")
			wantIncomplete(t, r.name, err, r.limit)
			if !hasWarning(f.Info(), r.warn) {
				t.Errorf("the cut %q left no %q warning: %v", r.name, r.warn, f.Info().Warnings)
			}
		})
	}
}

// TestDirectoryMapEndingShortIsACutAndWarns pins the one cause of the table above that no image reaches (a sound
// map is padded to the read limit and a damaged one has already recorded and logged its own cut): the check, on
// its helper. Mutation: drop the warning or the cut from cutIfShort, or log over an earlier cut; a row fails.
func TestDirectoryMapEndingShortIsACutAndWarns(t *testing.T) {
	dirInum := ext4test.InodeNumber(0) // /d
	for _, tc := range []struct {
		name        string
		lblk, isize int64
		first       string
		cut         string // the cut reason expected afterwards
		warns       bool
	}{
		{"covers i_size", 2, 2, "", "", false},
		{"beyond i_size", 3, 2, "", "", false},
		{"empty directory", 0, 0, "", "", false},
		{"ends short", 1, 2, "", "the block map ends at logical block 1, i_size covers 2", true},
		{"ends at once", 0, 5, "", "the block map ends at logical block 0, i_size covers 5", true},
		{"an earlier cut is kept and not logged again", 1, 2, "damaged", "damaged", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "b")))
			reason, err := f.CutIfShort(dirInum, tc.lblk, tc.isize, tc.first)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tc.cut {
				t.Errorf("cut = %q, want %q", reason, tc.cut)
			}
			if got := hasWarning(f.Info(), "the block map ends at logical block"); got != tc.warns {
				t.Errorf("warned = %v, want %v: %v", got, tc.warns, f.Info().Warnings)
			}
		})
	}
}
