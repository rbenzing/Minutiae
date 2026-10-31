package ext4_test

import (
	"bytes"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// These tests cover the Journaler methods JournalTransactions and JournalBlock (3G-R8, R7).

// jvCountingReader counts the reads that reach the image.
type jvCountingReader struct {
	*bytes.Reader
	reads atomic.Int64
}

func (r *jvCountingReader) ReadAt(p []byte, off int64) (int, error) {
	r.reads.Add(1)
	return r.Reader.ReadAt(p, off)
}

func jvOpen(t *testing.T, img []byte) (*ext4.FS, *jvCountingReader) {
	t.Helper()
	r := &jvCountingReader{Reader: bytes.NewReader(img)}
	f, err := ext4.Open(r, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return f, r
}

func jvTxns(t *testing.T, f *ext4.FS) []filesys.JournalTxn {
	t.Helper()
	var out []filesys.JournalTxn
	if err := f.JournalTransactions(func(tx filesys.JournalTxn) bool { out = append(out, tx); return true }); err != nil {
		t.Fatalf("JournalTransactions: %v", err)
	}
	return out
}

// jvImage: live 20 (two tags) and 21, stale 5 (two tags, the second overwritten so the copy is a mismatch, and a
// third that is missing because a foreign header sits in its slot is left to the stale tests).
func jvImage(t *testing.T) ([]byte, *ext4test.Layout) {
	t.Helper()
	stale := liveTxn(5, 100, 101)
	stale.At = 1
	stale.Tags[1].Overwrite = []byte("overwritten")
	return liveImage(t, 64, ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 20, Start: 10,
		Txns: []ext4test.JTxn{stale, {Seq: 20, At: 10, Tags: liveTxn(20, 110, 111).Tags}, liveTxn(21, 112)},
	})
}

func TestJournalTransactionsOrderAndFreshSlices(t *testing.T) {
	img, _ := jvImage(t)
	f := mustOpen(t, img)
	first := jvTxns(t, f)
	if len(first) != 3 {
		t.Fatalf("%d transactions; want 3", len(first))
	}
	var order []string
	for _, tx := range first {
		order = append(order, tx.Region)
	}
	if !reflect.DeepEqual(order, []string{"live", "live", "stale"}) || first[0].Seq != 20 || first[1].Seq != 21 || first[2].Seq != 5 {
		t.Errorf("order %v seqs %d %d %d; want live 20, live 21, stale 5", order, first[0].Seq, first[1].Seq, first[2].Seq)
	}
	s := first[2]
	if s.StartBlock != 1 || s.CommitBlock != 4 || !s.Committed || s.Checksum != "ok" || len(s.Blocks) != 2 {
		t.Errorf("stale txn = %+v; want start 1 commit 4 committed checksum ok, two tags", s)
	}
	if g := s.Blocks[0]; g.FSBlock != 100 || g.JournalBlock != 2 || g.Copy != "verified" || g.Escaped || g.Revoked {
		t.Errorf("stale tag 0 = %+v", g)
	}
	if g := s.Blocks[1]; g.Copy != "mismatch" || g.JournalBlock != 3 {
		t.Errorf("stale tag 1 = %+v; want a mismatch at journal block 3", g)
	}
	// Mutating a result must not change the next one.
	first[0].Blocks[0].FSBlock = 999
	first[0].Blocks = append(first[0].Blocks[:0], filesys.JournalTag{})
	first[0].Warnings = append(first[0].Warnings, "forged")
	first[2].Revoked = append(first[2].Revoked, 1)
	second := jvTxns(t, f)
	if second[0].Blocks[0].FSBlock != 110 || len(second[0].Blocks) != 2 || hasSub(second[0].Warnings, "forged") || len(second[2].Revoked) != 0 {
		t.Errorf("a second call sees the caller's edits: %+v", second)
	}
}

func TestJournalTransactionsVisitStops(t *testing.T) {
	img, _ := jvImage(t)
	f := mustOpen(t, img)
	n := 0
	if err := f.JournalTransactions(func(filesys.JournalTxn) bool { n++; return n < 2 }); err != nil || n != 2 {
		t.Errorf("visit returned false at the second call: %d visits, %v; want 2 and no error", n, err)
	}
	// A filesystem with no journal says so.
	noj, _ := ext4test.BuildLayout(ext4test.Options{Extents: true, MetadataCsum: true, Bit64: true, BlocksPerGroup: 2048}, nil)
	err := mustOpen(t, noj).JournalTransactions(func(filesys.JournalTxn) bool { return true })
	if !errors.Is(err, filesys.ErrNoJournal) {
		t.Errorf("no journal: JournalTransactions = %v; want ErrNoJournal", err)
	}
}

func TestJournalBlockReresolves(t *testing.T) {
	img, l := jvImage(t)
	f, r := jvOpen(t, img)
	txns := jvTxns(t, f)
	live, stale := txns[0], txns[2]
	for _, tc := range []struct {
		name string
		tx   filesys.JournalTxn
		i    int
		slot int // journal block whose bytes are expected
	}{{"live", live, 1, 12}, {"stale", stale, 0, 2}, {"stale mismatch", stale, 1, 3}} {
		got, run, err := f.JournalBlock(tc.tx, tc.i)
		if err != nil {
			t.Fatalf("%s: JournalBlock: %v", tc.name, err)
		}
		fsb := l.JournalFSBlock(tc.slot)
		if want := img[fsb*jtBS : (fsb+1)*jtBS]; !bytes.Equal(got, want) {
			t.Errorf("%s: bytes differ from journal block %d", tc.name, tc.slot)
		}
		if run != (filesys.Run{Offset: int64(fsb) * jtBS, Length: jtBS}) {
			t.Errorf("%s: run %+v; want {%d, %d} (filesystem-relative)", tc.name, run, int64(fsb)*jtBS, jtBS)
		}
	}
	// Whatever the bytes are, they are the caller's: editing them does not change the next read.
	a, _, _ := f.JournalBlock(live, 0)
	a[0] ^= 0xFF
	if b, _, _ := f.JournalBlock(live, 0); b[0] == a[0] {
		t.Error("JournalBlock returned a shared buffer")
	}

	forge := func(edit func(tx *filesys.JournalTxn)) filesys.JournalTxn {
		cp := live
		cp.Blocks = append([]filesys.JournalTag(nil), live.Blocks...)
		edit(&cp)
		return cp
	}
	before := r.reads.Load()
	for _, tc := range []struct {
		name string
		tx   filesys.JournalTxn
		i    int
	}{
		{"forged sequence", forge(func(tx *filesys.JournalTxn) { tx.Seq++ }), 0},
		{"forged region", forge(func(tx *filesys.JournalTxn) { tx.Region = "stale" }), 0},
		{"forged start block", forge(func(tx *filesys.JournalTxn) { tx.StartBlock++ }), 0},
		{"forged tag block", forge(func(tx *filesys.JournalTxn) { tx.Blocks[0].FSBlock++ }), 0},
		{"forged journal block", forge(func(tx *filesys.JournalTxn) { tx.Blocks[0].JournalBlock++ }), 0},
		{"index below range", live, -1},
		{"index above range", live, len(live.Blocks)},
		{"empty transaction", filesys.JournalTxn{}, 0},
	} {
		if b, _, err := f.JournalBlock(tc.tx, tc.i); !errors.Is(err, filesys.ErrNotFound) || b != nil {
			t.Errorf("%s: JournalBlock = %d bytes, %v; want ErrNotFound", tc.name, len(b), err)
		}
	}
	if after := r.reads.Load(); after != before {
		t.Errorf("rejected requests read the image %d times; want 0", after-before)
	}

	// A transaction from another image does not match this journal.
	other, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 50, Start: 1, Txns: []ext4test.JTxn{liveTxn(50, 110, 111)}})
	of := mustOpen(t, other)
	otx := jvTxns(t, of)[0]
	if _, _, err := f.JournalBlock(otx, 0); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a transaction of another image: %v; want ErrNotFound", err)
	}
}

func TestJournalBlockMissingAndOutsideAreNotFound(t *testing.T) {
	tx := liveTxn(7, 100, 101, 1<<40)
	tx.Tags[1].Overwrite = jHeader(2, 99)
	img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, Sequence: 30, Start: 0, Txns: []ext4test.JTxn{tx}})
	f, r := jvOpen(t, img)
	txns := jvTxns(t, f)
	st := txns[0]
	if len(st.Blocks) != 3 || st.Blocks[1].Copy != "missing" || st.Blocks[2].Copy != "missing" {
		t.Fatalf("stale txn %+v; want tags verified/unverified, missing, missing", st)
	}
	before := r.reads.Load()
	for i := 1; i < 3; i++ {
		if _, _, err := f.JournalBlock(st, i); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("tag %d (%s): %v; want ErrNotFound", i, st.Blocks[i].Copy, err)
		}
	}
	if r.reads.Load() != before {
		t.Error("a missing tag read the image")
	}
	// An outside tag, on a readable slot.
	out := liveTxn(8, 100)
	out.Tags = append(out.Tags, ext4test.JTag{FSBlock: 1 << 40, Data: liveData(9)})
	img, _ = liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 30, Start: 0, Txns: []ext4test.JTxn{out}})
	f = mustOpen(t, img)
	ot := jvTxns(t, f)[0]
	if len(ot.Blocks) != 2 || ot.Blocks[1].Copy != "outside" {
		t.Fatalf("txn %+v; want the second tag outside", ot)
	}
	if _, _, err := f.JournalBlock(ot, 1); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("an outside tag: %v; want ErrNotFound", err)
	}
	if _, _, err := f.JournalBlock(ot, 0); err != nil {
		t.Errorf("the good tag next to it: %v", err)
	}
}

func TestJournalBlockEscapedIsExact(t *testing.T) {
	magic := bytes.Repeat([]byte{0x5A}, jtBS)
	copy(magic, []byte{0xC0, 0x3B, 0x39, 0x98})
	img, l := liveImage(t, 64, ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 1, Start: 1,
		Txns: []ext4test.JTxn{{Seq: 1, At: -1, Tags: []ext4test.JTag{{FSBlock: 100, Data: magic}}}},
	})
	f := mustOpen(t, img)
	tx := jvTxns(t, f)[0]
	if !tx.Blocks[0].Escaped {
		t.Fatal("the escaped tag is not marked Escaped")
	}
	got, run, err := f.JournalBlock(tx, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte{0, 0, 0, 0}, magic[4:]...)
	if !bytes.Equal(got, want) {
		t.Errorf("the escaped block's first bytes are % x; want the zeros kept as logged (the decoders restore them, not the Journaler)", got[:8])
	}
	if fsb := l.JournalFSBlock(2); run.Offset != int64(fsb)*jtBS || run.Length != jtBS {
		t.Errorf("run %+v; want the slot's own block", run)
	}
}
