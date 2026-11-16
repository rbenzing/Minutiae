package ext4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// These tests build the journal with ext4test (an independent encoder) and read it back through ext4.FS.Journal.

var jtUUID = [16]byte{0xA1, 0xB2, 0xC3, 0xD4, 0xE5, 0xF6, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

const jtBS = 1024

func jtOpts(blocks int) ext4test.Options {
	return ext4test.Options{
		Extents: true, MetadataCsum: true, Bit64: true,
		UUID: [16]byte{9, 8, 7, 6, 5, 4, 3, 2, 1}, JournalBlocks: blocks, BlocksPerGroup: 2048,
	}
}

// jtImage builds a filesystem whose journal inode holds o.JournalBlocks journal blocks, writes j into it, and
// returns the image.
func jtImage(t *testing.T, o ext4test.Options, j ext4test.JBD2) ([]byte, *ext4test.Layout) {
	t.Helper()
	img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/a.txt", Data: pattern(3000, 1)}})
	if o.JournalBlocks > 0 {
		j.UUID = jtUUID
		l.WriteJournal(img, j)
	}
	return img, l
}

// jtJournal calls Journal and asserts that it added no filesystem warning.
func jtJournal(t *testing.T, img []byte) (filesys.JournalInfo, error) {
	t.Helper()
	f := mustOpen(t, img)
	before := f.Info().Warnings
	info, err := f.Journal()
	if after := f.Info().Warnings; !reflect.DeepEqual(before, after) {
		t.Errorf("Journal added filesystem warnings: before %q, after %q", before, after)
	}
	return info, err
}

func jtWant(t *testing.T, img []byte) filesys.JournalInfo {
	t.Helper()
	info, err := jtJournal(t, img)
	if err != nil {
		t.Fatalf("Journal: %v", err)
	}
	return info
}

func jtBE32(b []byte, off int, v uint32) { binary.BigEndian.PutUint32(b[off:], v) }
func jtBE(b []byte, off int) uint32      { return binary.BigEndian.Uint32(b[off:]) }

// jtPatchSuper edits journal block 0 in place.
func jtPatchSuper(img []byte, l *ext4test.Layout, edit func(sb []byte)) {
	fs := l.JournalFSBlock(0)
	edit(img[fs*jtBS : (fs+1)*jtBS])
}

func hasSub(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestJournalInfoOfBuilderJournal(t *testing.T) {
	// A kernel-mounted journal: v2, 64bit and csum v3 (the kernel adds revoke at the first revoke record).
	img, _ := jtImage(t, jtOpts(64), ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 7,
		Txns: []ext4test.JTxn{{Seq: 7, At: -1, Revokes: []uint64{100}}},
	})
	info := jtWant(t, img)
	want := filesys.JournalInfo{
		Type: "jbd2", Version: 2, BlockSize: jtBS, Blocks: 64, First: 1, Sequence: 7, Start: 0,
		Features: []string{"revoke", "64bit", "csum_v3"},
	}
	if !reflect.DeepEqual(info, want) {
		t.Errorf("Journal() = %+v\nwant %+v", info, want)
	}

	// A fresh mke2fs journal (3G-R2): v2 with no features, no checksum and no warning.
	img, _ = jtImage(t, jtOpts(64), ext4test.JBD2{Sequence: 1})
	info = jtWant(t, img)
	want = filesys.JournalInfo{Type: "jbd2", Version: 2, BlockSize: jtBS, Blocks: 64, First: 1, Sequence: 1}
	if !reflect.DeepEqual(info, want) {
		t.Errorf("fresh Journal() = %+v\nwant %+v", info, want)
	}
	if len(info.Features) != 0 {
		t.Errorf("a fresh journal reports features %q; want none", info.Features)
	}

	// Feature names, in bit order, compat first.
	img, _ = jtImage(t, jtOpts(400), ext4test.JBD2{
		CompatChecksum: true, AsyncCommit: true, Bit64: true, CsumV2: true, NumFC: 8,
		Txns: []ext4test.JTxn{{Seq: 1, At: -1, Revokes: []uint64{1}}},
	})
	if got, want := jtWant(t, img).Features, []string{"checksum", "revoke", "64bit", "async_commit", "csum_v2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Features = %q, want %q", got, want)
	}
}

func TestJournalNeedsRecoveryAndStart(t *testing.T) {
	img, l := jtImage(t, jtOpts(64), ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 63, Start: 1})
	l.PatchSuper(img, func(sb []byte) { binary.LittleEndian.PutUint32(sb[0x60:], binary.LittleEndian.Uint32(sb[0x60:])|0x4) })
	info := jtWant(t, img)
	if !info.NeedsRecovery || info.Start != 1 || info.Sequence != 63 {
		t.Errorf("NeedsRecovery %v Start %d Sequence %d; want true 1 63", info.NeedsRecovery, info.Start, info.Sequence)
	}
	if hasSub(info.Warnings, "needs_recovery") {
		t.Errorf("a marked filesystem warns about needs_recovery: %q", info.Warnings)
	}
}

func TestJournalV1Superblock(t *testing.T) {
	img, _ := jtImage(t, jtOpts(64), ext4test.JBD2{Version: 1, Sequence: 3})
	info := jtWant(t, img)
	want := filesys.JournalInfo{Type: "jbd2", Version: 1, BlockSize: jtBS, Blocks: 64, First: 1, Sequence: 3}
	if !reflect.DeepEqual(info, want) {
		t.Errorf("v1 Journal() = %+v\nwant %+v", info, want)
	}
}

func TestJournalBlockMapJournal(t *testing.T) {
	// An ext3-style journal inode (a block map, no extent feature) is located the same way.
	o := jtOpts(64)
	o.Extents, o.MetadataCsum, o.Bit64 = false, false, false
	img, _ := jtImage(t, o, ext4test.JBD2{Sequence: 2})
	info := jtWant(t, img)
	if info.Version != 2 || info.Blocks != 64 || info.Sequence != 2 || len(info.Features) != 0 {
		t.Errorf("block-map journal: %+v", info)
	}
}

func TestJournalExt2IsNoJournal(t *testing.T) {
	img, _ := jtImage(t, ext4test.Options{Extents: true}, ext4test.JBD2{})
	_, err := jtJournal(t, img)
	if !errors.Is(err, filesys.ErrNoJournal) {
		t.Fatalf("Journal on a filesystem without has_journal = %v; want ErrNoJournal", err)
	}
}

func TestJournalExternalIsUnsupported(t *testing.T) {
	// has_journal with s_journal_inum 0: the journal is on another device.
	img, _ := jtImage(t, ext4test.Options{Extents: true, Journal: true}, ext4test.JBD2{})
	_, err := jtJournal(t, img)
	if !errors.Is(err, filesys.ErrUnsupported) || errors.Is(err, filesys.ErrNoJournal) {
		t.Fatalf("Journal with s_journal_inum 0 = %v; want ErrUnsupported and not ErrNoJournal", err)
	}
	if err != nil && !strings.Contains(err.Error(), "external journal") {
		t.Errorf("error %q does not say the journal is external", err)
	}
}

func TestJournalUnknownIncompatIsUnsupported(t *testing.T) {
	img, _ := jtImage(t, jtOpts(64), ext4test.JBD2{UnknownIncompat: 0x140})
	_, err := jtJournal(t, img)
	if !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("unknown incompat bits = %v; want ErrUnsupported", err)
	}
	if err != nil && !strings.Contains(err.Error(), "0x140") {
		t.Errorf("error %q does not name the bits 0x140", err)
	}
}

func TestJournalBothCsumVersionsIsCorrupt(t *testing.T) {
	img, _ := jtImage(t, jtOpts(64), ext4test.JBD2{CsumV2: true, CsumV3: true})
	_, err := jtJournal(t, img)
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("csum v2 and v3 together = %v; want a corrupt error", err)
	}
}

func TestJournalInodeHoleIsCorrupt(t *testing.T) {
	img, l := jtImage(t, jtOpts(64), ext4test.JBD2{Sequence: 1})
	// Shorten the single extent to 32 blocks: journal blocks 32..63 are a hole.
	l.PatchInode(img, l.JournalInode, func(raw []byte) { binary.LittleEndian.PutUint16(raw[0x28+12+4:], 32) })
	_, err := jtJournal(t, img)
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("journal inode with a hole = %v; want a corrupt error", err)
	}
	if err != nil && !strings.Contains(err.Error(), "journal block 32") {
		t.Errorf("error %q does not name journal block 32", err)
	}
}

func TestJournalInodeMustBeAllocatedRegularFile(t *testing.T) {
	for name, edit := range map[string]func(img []byte, l *ext4test.Layout){
		"not allocated": func(img []byte, l *ext4test.Layout) { l.SetInodeFree(img, l.JournalInode, true) },
		"links 0": func(img []byte, l *ext4test.Layout) {
			l.PatchInode(img, l.JournalInode, func(raw []byte) { binary.LittleEndian.PutUint16(raw[0x1A:], 0) })
		},
		"a directory": func(img []byte, l *ext4test.Layout) {
			l.PatchInode(img, l.JournalInode, func(raw []byte) { binary.LittleEndian.PutUint16(raw[0:], 0x41ED) })
		},
		"no blocks": func(img []byte, l *ext4test.Layout) {
			l.PatchInode(img, l.JournalInode, func(raw []byte) { binary.LittleEndian.PutUint32(raw[0x4:], 0) })
		},
		"inline data": func(img []byte, l *ext4test.Layout) {
			l.PatchInode(img, l.JournalInode, func(raw []byte) {
				binary.LittleEndian.PutUint32(raw[0x20:], binary.LittleEndian.Uint32(raw[0x20:])|0x10000000)
			})
		},
		"out of range": func(img []byte, l *ext4test.Layout) {
			l.PatchSuper(img, func(sb []byte) { binary.LittleEndian.PutUint32(sb[0xE0:], 0x7FFFFFFF) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			img, l := jtImage(t, jtOpts(64), ext4test.JBD2{Sequence: 1})
			edit(img, l)
			_, err := jtJournal(t, img)
			if !errors.Is(err, filesys.ErrCorrupt) {
				t.Fatalf("Journal = %v; want a corrupt error", err)
			}
		})
	}
}

func TestJournalAllocationUnknownWarns(t *testing.T) {
	// A damaged inode bitmap cannot say whether inode 8 is allocated: that is reported, not assumed.
	img, l := jtImage(t, jtOpts(64), ext4test.JBD2{Sequence: 1})
	img[l.InodeBitmap[0]*jtBS+15] ^= 0x10 // a bit of an unrelated inode; the bitmap checksum no longer verifies
	info := jtWant(t, img)
	if !hasSub(info.Warnings, "allocat") {
		t.Errorf("warnings %q do not say the allocation of the journal inode is unknown", info.Warnings)
	}
}

func TestJournalSuperblockFieldsAreChecked(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(sb []byte)
	}{
		{"maxlen zero", func(sb []byte) { jtBE32(sb, 0x10, 0) }},
		{"bad magic", func(sb []byte) { jtBE32(sb, 0, 0x12345678) }},
		{"not a superblock type", func(sb []byte) { jtBE32(sb, 4, 1) }},
		{"first zero", func(sb []byte) { jtBE32(sb, 0x14, 0) }},
		{"first at maxlen", func(sb []byte) { jtBE32(sb, 0x14, 64) }},
		{"start below first", func(sb []byte) { jtBE32(sb, 0x14, 4); jtBE32(sb, 0x1C, 2) }},
		{"start at maxlen", func(sb []byte) { jtBE32(sb, 0x1C, 64) }},
		{"unsupported checksum type", func(sb []byte) { sb[0x50] = 1 }},
		{"fast commit area as large as the journal", func(sb []byte) { jtBE32(sb, 0x28, jtBE(sb, 0x28)|0x20); jtBE32(sb, 0x54, 64) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			img, l := jtImage(t, jtOpts(64), ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1})
			jtPatchSuper(img, l, c.edit)
			_, err := jtJournal(t, img)
			if !errors.Is(err, filesys.ErrCorrupt) && !errors.Is(err, filesys.ErrUnsupported) {
				t.Fatalf("Journal = %v; want a corrupt (or unsupported) error", err)
			}
		})
	}
}

func TestJournalMaxLenBeyondInodeIsCorrupt(t *testing.T) {
	img, l := jtImage(t, jtOpts(64), ext4test.JBD2{Sequence: 1})
	jtPatchSuper(img, l, func(sb []byte) { jtBE32(sb, 0x10, 65) })
	_, err := jtJournal(t, img)
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("s_maxlen 65 over a 64-block inode = %v; want a corrupt error", err)
	}
}

func TestJournalBlockSizeMismatchIsCorrupt(t *testing.T) {
	img, l := jtImage(t, jtOpts(64), ext4test.JBD2{Sequence: 1})
	jtPatchSuper(img, l, func(sb []byte) { jtBE32(sb, 0xC, 4096) })
	_, err := jtJournal(t, img)
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("s_blocksize 4096 on a 1 KiB filesystem = %v; want a corrupt error", err)
	}
}

func TestJournalSuperChecksumMismatchWarns(t *testing.T) {
	img, _ := jtImage(t, jtOpts(64), ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1, BadSuperCsum: true})
	f := mustOpen(t, img)
	before := f.Info().Warnings
	info, err := f.Journal()
	if err != nil {
		t.Fatalf("a wrong superblock checksum is a warning, not an error: %v", err)
	}
	if !hasSub(info.Warnings, "checksum") {
		t.Errorf("JournalInfo warnings %q do not report the superblock checksum", info.Warnings)
	}
	if !reflect.DeepEqual(before, f.Info().Warnings) {
		t.Errorf("Info().Warnings changed: %q -> %q", before, f.Info().Warnings)
	}
	// The same journal with a good checksum is silent.
	img, _ = jtImage(t, jtOpts(64), ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1})
	if w := jtWant(t, img).Warnings; len(w) != 0 {
		t.Errorf("a good journal warns: %q", w)
	}
}

func TestJournalTooLargeIsLimit(t *testing.T) {
	img, l := jtImage(t, jtOpts(64), ext4test.JBD2{Sequence: 1})
	f := mustOpen(t, img)
	f.SetMaxJournalBytes(32 * jtBS)
	if _, err := f.Journal(); !errors.Is(err, filesys.ErrLimit) {
		t.Fatalf("a 64-block journal under a 32-block cap = %v; want ErrLimit", err)
	}
	f = mustOpen(t, img)
	f.SetMaxJournalBytes(64 * jtBS)
	if _, err := f.Journal(); err != nil {
		t.Fatalf("a journal exactly at the cap: %v", err)
	}
	// s_maxlen alone reaches the cap too when the inode is larger than the journal it holds.
	jtPatchSuper(img, l, func(sb []byte) { jtBE32(sb, 0x10, 40) })
	f = mustOpen(t, img)
	f.SetMaxJournalBytes(64 * jtBS)
	if info, err := f.Journal(); err != nil || info.Blocks != 40 {
		t.Fatalf("s_maxlen 40 inside a 64-block inode: %+v, %v", info, err)
	}
}

func TestJBD2FastCommitIsIncomplete(t *testing.T) {
	// s_num_fc_blks 0 means the default of 256 blocks.
	img, _ := jtImage(t, jtOpts(400), ext4test.JBD2{Bit64: true, CsumV3: true, FastCommit: true, Sequence: 1})
	info := jtWant(t, img)
	if !info.Incomplete || info.Stop != "fast-commit-not-analysed" {
		t.Errorf("Incomplete %v Stop %q; want true fast-commit-not-analysed", info.Incomplete, info.Stop)
	}
	if !hasSub(info.Warnings, "256 blocks") {
		t.Errorf("warnings %q do not name 256 blocks", info.Warnings)
	}
	img, _ = jtImage(t, jtOpts(400), ext4test.JBD2{Bit64: true, CsumV3: true, FastCommit: true, NumFC: 8, Sequence: 1})
	if info := jtWant(t, img); !info.Incomplete || !hasSub(info.Warnings, "8 blocks") {
		t.Errorf("NumFC 8: Incomplete %v warnings %q", info.Incomplete, info.Warnings)
	}
	// Without the feature the journal is complete.
	img, _ = jtImage(t, jtOpts(400), ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1})
	if info := jtWant(t, img); info.Incomplete || info.Stop != "" {
		t.Errorf("no fast commit: Incomplete %v Stop %q", info.Incomplete, info.Stop)
	}
}

func TestJournalLiveRegionWithoutNeedsRecoveryWarns(t *testing.T) {
	img, _ := jtImage(t, jtOpts(64), ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 5, Start: 3})
	info := jtWant(t, img)
	if !hasSub(info.Warnings, "live region but the filesystem is not marked needs_recovery") {
		t.Errorf("warnings %q do not report a live region on an unmarked filesystem", info.Warnings)
	}
	// A clean journal on an unmarked filesystem is silent.
	img, _ = jtImage(t, jtOpts(64), ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 5})
	if w := jtWant(t, img).Warnings; len(w) != 0 {
		t.Errorf("a clean journal warns: %q", w)
	}
}

func TestJournalAddsNoFilesystemWarning(t *testing.T) {
	// jtJournal asserts Info().Warnings is unchanged; run it over damaged and odd journals too.
	cases := map[string]func() []byte{
		"bad super checksum": func() []byte {
			img, _ := jtImage(t, jtOpts(64), ext4test.JBD2{CsumV3: true, BadSuperCsum: true})
			return img
		},
		"fast commit": func() []byte {
			img, _ := jtImage(t, jtOpts(400), ext4test.JBD2{CsumV3: true, FastCommit: true})
			return img
		},
		"live region": func() []byte {
			img, _ := jtImage(t, jtOpts(64), ext4test.JBD2{Start: 2})
			return img
		},
		"hole": func() []byte {
			img, l := jtImage(t, jtOpts(64), ext4test.JBD2{})
			l.PatchInode(img, l.JournalInode, func(raw []byte) { binary.LittleEndian.PutUint16(raw[0x28+12+4:], 3) })
			return img
		},
		"ext2":     func() []byte { img, _ := jtImage(t, ext4test.Options{}, ext4test.JBD2{}); return img },
		"external": func() []byte { img, _ := jtImage(t, ext4test.Options{Journal: true}, ext4test.JBD2{}); return img },
		"extents flag without the feature": func() []byte {
			// the plain inode decoder warns about this flag; the journal's quiet decoder must not
			o := jtOpts(64)
			o.Extents, o.MetadataCsum, o.Bit64 = false, false, false
			img, l := jtImage(t, o, ext4test.JBD2{})
			l.PatchInode(img, l.JournalInode, func(raw []byte) {
				binary.LittleEndian.PutUint32(raw[0x20:], binary.LittleEndian.Uint32(raw[0x20:])|0x80000)
			})
			return img
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) { _, _ = jtJournal(t, mk()) })
	}
}

// TestJournalUsesQuietView damages the checksum of the extent block of a depth-1 journal inode: the damage is the
// journal's finding, not the filesystem's.
func TestJournalUsesQuietView(t *testing.T) {
	for _, good := range []bool{true, false} {
		img, l := jtImage(t, jtOpts(64), ext4test.JBD2{Sequence: 1})
		leaf := len(img)/jtBS - 1
		l.WriteLeaf(img, leaf, l.JournalInode, 0, []ext4test.Extent{{Lblk: 0, Len: 64, Phys: l.JournalFSBlock(0)}}, good)
		l.PatchInode(img, l.JournalInode, func(raw []byte) {
			ib := raw[0x28 : 0x28+60]
			clear(ib)
			binary.LittleEndian.PutUint16(ib[0:], 0xF30A)
			binary.LittleEndian.PutUint16(ib[2:], 1) // entries
			binary.LittleEndian.PutUint16(ib[4:], 4) // max
			binary.LittleEndian.PutUint16(ib[6:], 1) // depth
			binary.LittleEndian.PutUint32(ib[16:], uint32(leaf))
		})
		f := mustOpen(t, img)
		before := f.Info().Warnings
		info, err := f.Journal()
		if after := f.Info().Warnings; !reflect.DeepEqual(before, after) {
			t.Errorf("good=%v: Journal added filesystem warnings: %q -> %q", good, before, after)
		}
		if good {
			if err != nil || len(info.Warnings) != 0 || info.Blocks != 64 {
				t.Errorf("an intact depth-1 journal inode: %+v, %v", info, err)
			}
			continue
		}
		if err == nil && !hasSub(info.Warnings, "checksum") {
			t.Errorf("a damaged extent block gives neither an error nor a journal warning: %+v", info)
		}
	}
}

func TestJournalLocatedOnce(t *testing.T) {
	img, _ := jtImage(t, jtOpts(64), ext4test.JBD2{Sequence: 1})
	f := mustOpen(t, img)
	first, err := f.Journal()
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		got, err := f.Journal()
		if err != nil || !reflect.DeepEqual(got, first) {
			t.Fatalf("repeat Journal = %+v, %v; want %+v", got, err, first)
		}
	}
	if builds, _ := f.JournalStats(); builds != 1 {
		t.Errorf("the journal was located %d times; want once", builds)
	}
	// The answer is a copy: changing it changes nothing.
	first.Features = append(first.Features, "x")
	first.Warnings = append(first.Warnings, "x")
	if again, _ := f.Journal(); len(again.Features) != 0 || len(again.Warnings) != 0 {
		t.Errorf("a caller changed the cached answer: %+v", again)
	}

	// A corrupt result is cached too: the second call is answered without another build.
	img, l := jtImage(t, jtOpts(64), ext4test.JBD2{Sequence: 1})
	jtPatchSuper(img, l, func(sb []byte) { jtBE32(sb, 0, 0) })
	f = mustOpen(t, img)
	_, e1 := f.Journal()
	_, e2 := f.Journal()
	if !errors.Is(e1, filesys.ErrCorrupt) || e2 == nil || e1.Error() != e2.Error() {
		t.Errorf("corrupt journal: %v then %v", e1, e2)
	}
	if builds, _ := f.JournalStats(); builds != 1 {
		t.Errorf("a corrupt journal was located %d times; want once", builds)
	}
}

// jtFailReader fails every read that reaches from once armed.
type jtFailReader struct {
	*bytes.Reader
	from  int64
	armed bool
}

func (r *jtFailReader) ReadAt(p []byte, off int64) (int, error) {
	if r.armed && off+int64(len(p)) > r.from {
		return 0, errors.New("injected read failure")
	}
	return r.Reader.ReadAt(p, off)
}

func TestJournalIOErrorIsNotCached(t *testing.T) {
	img, l := jtImage(t, jtOpts(64), ext4test.JBD2{Sequence: 1})
	r := &jtFailReader{Reader: bytes.NewReader(img), from: int64(l.JournalFSBlock(0)) * jtBS}
	f, err := ext4.Open(r, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	r.armed = true
	_, err = f.Journal()
	if err == nil || errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, filesys.ErrUnsupported) || errors.Is(err, filesys.ErrLimit) {
		t.Fatalf("a failing read = %v; want the I/O error itself, not a corrupt/unsupported/limit verdict", err)
	}
	r.armed = false
	if info, err := f.Journal(); err != nil || info.Version != 2 {
		t.Fatalf("after the read works again: %+v, %v; the failure must not have been cached", info, err)
	}
}

// jtOnceFailReader fails the first read that touches [from, to) once armed, then reads normally.
type jtOnceFailReader struct {
	*bytes.Reader
	from, to int64
	armed    bool
	failed   int
}

func (r *jtOnceFailReader) ReadAt(p []byte, off int64) (int, error) {
	if r.armed && off < r.to && off+int64(len(p)) > r.from {
		r.armed = false
		r.failed++
		return 0, errors.New("injected read failure")
	}
	return r.Reader.ReadAt(p, off)
}

// TestJournalExtentWalkIOErrorIsNotCached: a failed read of an extent-tree node of the journal inode is an I/O error,
// not a corrupt journal, and is not cached (Task 4 review follow-up).
func TestJournalExtentWalkIOErrorIsNotCached(t *testing.T) {
	o := jtOpts(64)
	o.JournalExtentLeaves = 1
	img, l := jtImage(t, o, ext4test.JBD2{Sequence: 1})
	// The root's only index entry names the leaf block (ei_leaf_lo at i_block+12+4).
	var leaf int64
	l.PatchInode(img, l.JournalInode, func(raw []byte) {
		if d := binary.LittleEndian.Uint16(raw[0x28+6:]); d != 1 {
			t.Fatalf("journal inode extent depth = %d; want 1", d)
		}
		leaf = int64(binary.LittleEndian.Uint32(raw[0x28+12+4:]))
	})
	r := &jtOnceFailReader{Reader: bytes.NewReader(img), from: leaf * jtBS, to: (leaf + 1) * jtBS}
	f, err := ext4.Open(r, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	r.armed = true
	_, err = f.Journal()
	if r.failed != 1 {
		t.Fatalf("the injected failure fired %d times; want 1 (the leaf was not read)", r.failed)
	}
	if err == nil || errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, filesys.ErrUnsupported) || errors.Is(err, filesys.ErrLimit) {
		t.Fatalf("a failed extent-node read = %v; want the I/O error, not a corrupt/unsupported/limit verdict", err)
	}
	info, err := f.Journal()
	if err != nil || info.Version != 2 {
		t.Fatalf("after the read works again: %+v, %v; the failure must not have been cached", info, err)
	}
}
