package ext4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// FuzzJBD2 patches bytes into the journal of a 64-block builder image and holds the journal reader to its bounds
// (3G-R9): no panic; Journal and JournalTransactions return a typed error or a bounded result; every tag names a
// block inside the filesystem or is flagged outside or missing; JournalBlock returns ErrNotFound or one block with
// a run inside the image; a second call gives equal results; the filesystem warnings and Unallocated are unchanged.
//
// Input: byte 0 picks the base journal (bit 0 clear: checksum v3 with 64bit tags; set: no checksums, so an edit
// reaches the parser instead of stopping at a checksum), then 3-byte edits (offset in the journal, little endian;
// value). Gate run (3G Task 7):
//
//	go test ./internal/filesys/ext4 -run '^$' -fuzz '^FuzzJBD2$' -fuzztime 30s -fuzzminimizetime 0 -parallel 2

type fuzzBase struct {
	img   [2][]byte // checksum v3, and no checksums
	jroot int       // byte offset of journal block 0 (the journal is contiguous in the builder image)
}

var (
	fuzzBaseOnce sync.Once
	fuzzBases    fuzzBase
)

// fuzzJBD2Base builds the two base images once: stale transactions (one slot overwritten, one block escaped) in
// journal blocks 1..5, and a live region from block 10: a revoke block, a descriptor with two tags and a commit,
// then a second transaction.
func fuzzJBD2Base(t testing.TB) *fuzzBase {
	fuzzBaseOnce.Do(func() {
		tt, ok := t.(*testing.T)
		if !ok {
			tt = new(testing.T)
		}
		for variant := range 2 {
			stale := liveTxn(5, 100, 101, 102)
			stale.At = 1
			stale.Tags[1].Overwrite = []byte("overwritten")
			esc := make([]byte, jtBS)
			binary.BigEndian.PutUint32(esc, ext4test.JBD2Magic)
			stale.Tags[2].Data = esc // an escaped block
			live := liveTxn(20, 110, 111)
			live.At = 10
			live.Revokes = []uint64{100, 101}
			j := ext4test.JBD2{
				Bit64: true, CsumV3: variant == 0, Sequence: 20, Start: 10,
				Txns: []ext4test.JTxn{stale, live, liveTxn(21, 112)},
			}
			img, l := liveImage(tt, 64, j)
			fuzzBases.img[variant] = img
			fuzzBases.jroot = l.JournalFSBlock(0) * jtBS
		}
	})
	return &fuzzBases
}

// fuzzEdits encodes an input: the base variant and (journal offset, value) edits.
func fuzzEdits(variant byte, edits ...[2]int) []byte {
	out := []byte{variant}
	for _, e := range edits {
		out = append(out, byte(e[0]), byte(e[0]>>8), byte(e[1]))
	}
	return out
}

func FuzzJBD2(f *testing.F) {
	fuzzJBD2Base(f)
	f.Add([]byte{})
	for variant := range byte(2) {
		f.Add(fuzzEdits(variant))
		// Superblock fields (journal block 0, big endian): s_maxlen, s_first, s_sequence, s_start, s_blocksize.
		f.Add(fuzzEdits(variant, [2]int{0x10, 0xFF}))
		f.Add(fuzzEdits(variant, [2]int{0x17, 0}))
		f.Add(fuzzEdits(variant, [2]int{0x1B, 3}))
		f.Add(fuzzEdits(variant, [2]int{0x1F, 63}))
		f.Add(fuzzEdits(variant, [2]int{0x0E, 8}))
		// A tag of the stale descriptor (journal block 1): its block number and its flags.
		f.Add(fuzzEdits(variant, [2]int{jtBS + 12, 0xFF}))
		f.Add(fuzzEdits(variant, [2]int{jtBS + 12 + 7, 0x80}))
		// r_count of the live revoke block (journal block 10).
		f.Add(fuzzEdits(variant, [2]int{10*jtBS + 12, 0xFF}))
		f.Add(fuzzEdits(variant, [2]int{10*jtBS + 15, 0}))
		// The first checksum word of the live commit block (journal block 14).
		f.Add(fuzzEdits(variant, [2]int{14*jtBS + 16, 0x55}))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		b := fuzzJBD2Base(t)
		variant := 0
		if len(data) > 0 {
			variant = int(data[0] & 1)
			data = data[1:]
		}
		img := slices.Clone(b.img[variant])
		const journalBytes = 64 * jtBS
		for i := 0; i+3 <= len(data) && i < 3*256; i += 3 {
			off := int(binary.LittleEndian.Uint16(data[i:]))
			img[b.jroot+off%journalBytes] = data[i+2]
		}
		fuzzCheckJournal(t, img)
	})
}

func fuzzTypedErr(err error) bool {
	var ce *filesys.CorruptError
	return errors.As(err, &ce) || errors.Is(err, filesys.ErrUnsupported) || errors.Is(err, filesys.ErrLimit) ||
		errors.Is(err, filesys.ErrNoJournal) || errors.Is(err, filesys.ErrIncomplete)
}

func fuzzCheckJournal(t *testing.T, img []byte) {
	t.Helper()
	open := func() *ext4.FS {
		fs, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			return nil
		}
		return fs
	}
	fs := open()
	if fs == nil {
		return // the edit never reaches the filesystem structures; an open failure is not this target's concern
	}
	warnBefore := slices.Clone(fs.Info().Warnings)

	info, err := fs.Journal()
	if err != nil {
		if !fuzzTypedErr(err) {
			t.Fatalf("Journal: untyped error %v", err)
		}
		if _, err2 := fs.Journal(); err2 == nil || err2.Error() != err.Error() {
			t.Fatalf("a second Journal call gives %v after %v", err2, err)
		}
		return
	}
	var txns []filesys.JournalTxn
	if err := fs.JournalTransactions(func(tx filesys.JournalTxn) bool { txns = append(txns, tx); return true }); err != nil {
		t.Fatalf("JournalTransactions after a good Journal: %v", err)
	}
	if _, read := fs.JournalStats(); read > 2*info.Blocks {
		t.Fatalf("%d journal blocks read for a %d-block journal", read, info.Blocks)
	}
	if len(txns) > ext4.MaxJournalTxns {
		t.Fatalf("%d transactions, over the cap %d", len(txns), ext4.MaxJournalTxns)
	}
	fsBlocks := uint64(len(img) / jtBS)
	tags := 0
	for _, tx := range txns {
		tags += len(tx.Blocks)
		for i, g := range tx.Blocks {
			if g.JournalBlock >= uint64(info.Blocks) {
				t.Fatalf("tag %d of txn %d names journal block %d of a %d-block journal", i, tx.Seq, g.JournalBlock, info.Blocks)
			}
			switch g.Copy {
			case "verified", "unverified", "mismatch":
				if g.FSBlock >= fsBlocks {
					t.Fatalf("tag %d of txn %d names block %d beyond the %d-block filesystem with copy %q", i, tx.Seq, g.FSBlock, fsBlocks, g.Copy)
				}
			case "missing", "outside":
			default:
				t.Fatalf("tag %d of txn %d has copy state %q", i, tx.Seq, g.Copy)
			}
			blk, run, err := fs.JournalBlock(tx, i)
			switch g.Copy {
			case "missing", "outside":
				if !errors.Is(err, filesys.ErrNotFound) {
					t.Fatalf("JournalBlock of a %s tag = %v; want ErrNotFound", g.Copy, err)
				}
			default:
				if err != nil {
					t.Fatalf("JournalBlock of a %s tag: %v", g.Copy, err)
				}
				if len(blk) != jtBS || run.Length != jtBS || run.Offset < 0 || run.Offset%jtBS != 0 || run.Offset+run.Length > int64(len(img)) {
					t.Fatalf("JournalBlock = %d bytes, run %+v in a %d-byte image", len(blk), run, len(img))
				}
				if !bytes.Equal(blk, img[run.Offset:run.Offset+run.Length]) {
					t.Fatalf("JournalBlock bytes differ from the slot they came from")
				}
			}
		}
	}
	if tags > ext4.MaxJournalTags {
		t.Fatalf("%d tags, over the cap %d", tags, ext4.MaxJournalTags)
	}

	// A second call gives the same answers.
	info2, err := fs.Journal()
	if err != nil || !reflect.DeepEqual(info, info2) {
		t.Fatalf("a second Journal call differs: %+v, %v", info2, err)
	}
	var txns2 []filesys.JournalTxn
	_ = fs.JournalTransactions(func(tx filesys.JournalTxn) bool { txns2 = append(txns2, tx); return true })
	if !reflect.DeepEqual(txns, txns2) {
		t.Fatalf("a second JournalTransactions call differs")
	}

	// The live view is unchanged: the warnings, and the unallocated map against a filesystem that never saw the journal.
	if warnAfter := fs.Info().Warnings; !slices.Equal(warnBefore, warnAfter) {
		t.Fatalf("the journal changed the filesystem warnings: %q -> %q", warnBefore, warnAfter)
	}
	un1, e1 := fs.Unallocated()
	un2, e2 := open().Unallocated()
	if (e1 == nil) != (e2 == nil) || !reflect.DeepEqual(un1, un2) {
		t.Fatalf("Unallocated differs after journal calls")
	}
}

// The fuzz base must actually hold what the property is about: live and stale transactions, a revoke, an escaped
// block and an overwritten slot.
func TestFuzzJBD2BaseIsRich(t *testing.T) {
	b := fuzzJBD2Base(t)
	for variant, img := range b.img {
		f := mustOpen(t, img)
		var live, stale, escaped, mismatch, revoked int
		if err := f.JournalTransactions(func(tx filesys.JournalTxn) bool {
			if tx.Region == "live" {
				live++
			} else {
				stale++
			}
			for _, g := range tx.Blocks {
				if g.Escaped {
					escaped++
				}
				if g.Copy == "mismatch" || (variant == 1 && g.Copy == "unverified" && tx.Region == "stale" && g.FSBlock == 101) {
					mismatch++
				}
				if g.Revoked {
					revoked++
				}
			}
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if live != 2 || stale != 1 || escaped != 1 || mismatch != 1 || revoked != 2 {
			t.Errorf("variant %d: live %d stale %d escaped %d mismatch %d revoked %d; want 2, 1, 1, 1, 2", variant, live, stale, escaped, mismatch, revoked)
		}
	}
}
