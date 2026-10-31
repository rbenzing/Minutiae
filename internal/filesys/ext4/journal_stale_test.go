package ext4_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// These tests cover the stale pass (3G-R5), the revoke rule (3G-R6) and the copy states of stale tags. They read the
// index through ext4.IndexTxns (live transactions first, then stale fragments by start block).

func txnsOf(t *testing.T, f *ext4.FS) []ext4.IndexTxn {
	t.Helper()
	txns, _, err := f.IndexTxns()
	if err != nil {
		t.Fatalf("IndexTxns: %v", err)
	}
	return txns
}

func allTxns(t *testing.T, img []byte) []ext4.IndexTxn {
	t.Helper()
	return txnsOf(t, mustOpen(t, img))
}

// jHeader is a journal header block: the magic, a block type and a sequence, the rest zero.
func jHeader(typ, seq uint32) []byte {
	b := make([]byte, jtBS)
	binary.BigEndian.PutUint32(b[0:], ext4test.JBD2Magic)
	binary.BigEndian.PutUint32(b[4:], typ)
	binary.BigEndian.PutUint32(b[8:], seq)
	return b
}

func copies(tags []ext4.IndexTag) []string {
	var out []string
	for _, g := range tags {
		out = append(out, g.Copy)
	}
	return out
}

func TestJBD2StaleRegionMarked(t *testing.T) {
	// A clean journal (s_start 0) holding three earlier transactions: all of them are stale.
	txA := liveTxn(9, 100, 101)
	txA.CommitSec = 1_700_000_000
	img, _ := liveImage(t, 64, ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 12, Start: 0,
		Txns: []ext4test.JTxn{txA, liveTxn(10, 102, 103), liveTxn(11, 104, 105)},
	})
	txns := allTxns(t, img)
	if len(txns) != 3 {
		t.Fatalf("%d transactions in a clean journal of three; want 3", len(txns))
	}
	for i, tx := range txns {
		start := int64(1 + 4*i)
		if !tx.Stale || !tx.Committed || tx.Seq != uint32(9+i) || tx.Start != start || tx.Commit != start+3 || tx.Csum != "ok" {
			t.Errorf("txn %d = %+v; want stale committed seq %d start %d commit %d csum ok", i, tx, 9+i, start, start+3)
		}
		if got, want := fsBlocksOf(tx.Tags), []uint64{uint64(100 + 2*i), uint64(101 + 2*i)}; !reflect.DeepEqual(got, want) {
			t.Errorf("txn %d tags %v; want %v", i, got, want)
		}
		if got := copies(tx.Tags); !reflect.DeepEqual(got, []string{"verified", "verified"}) {
			t.Errorf("txn %d copy states %v; want verified", i, got)
		}
	}
	if txns[0].CommitTime.T.Unix() != 1_700_000_000 {
		t.Errorf("stale commit time %v; want 1700000000", txns[0].CommitTime)
	}

	// A crash journal: the live part is 20 and 21 at s_start 10; the older part (5, 6) is stale.
	img, _ = liveImage(t, 64, ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 20, Start: 10,
		Txns: []ext4test.JTxn{
			{Seq: 5, At: 1, Tags: liveTxn(5, 100).Tags},
			{Seq: 6, At: 5, Tags: liveTxn(6, 101).Tags},
			{Seq: 20, At: 10, Tags: liveTxn(20, 102).Tags},
			liveTxn(21, 103),
		},
	})
	var got []string
	for _, tx := range allTxns(t, img) {
		region := "live"
		if tx.Stale {
			region = "stale"
		}
		got = append(got, fmt.Sprintf("%s:%d", region, tx.Seq))
	}
	if want := []string{"live:20", "live:21", "stale:5", "stale:6"}; !reflect.DeepEqual(got, want) {
		t.Errorf("regions %v; want %v (live in log order, then stale by start block)", got, want)
	}
	// Through the Journaler: no stale transaction is live.
	var regions []string
	err := mustOpen(t, img).JournalTransactions(func(tx filesys.JournalTxn) bool {
		regions = append(regions, tx.Region)
		return true
	})
	if err != nil || !reflect.DeepEqual(regions, []string{"live", "live", "stale", "stale"}) {
		t.Errorf("JournalTransactions regions %v, %v; want live, live, stale, stale", regions, err)
	}
}

func TestStaleFragmentRule(t *testing.T) {
	// 7: a revoke, a descriptor with two slots and a commit (blocks 1-5), followed directly by 8: a descriptor, a slot
	// and a commit (blocks 6-8). 9 has no commit (blocks 10-11).
	t7 := liveTxn(7, 100, 101)
	t7.Revokes = []uint64{50}
	t9 := liveTxn(9, 103)
	t9.NoCommit = true
	t9.At = 10
	img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 30, Start: 0, Txns: []ext4test.JTxn{t7, liveTxn(8, 102), t9}})
	txns := allTxns(t, img)
	if len(txns) != 3 {
		t.Fatalf("%d fragments; want 3 (a commit directly followed by the next descriptor makes two)", len(txns))
	}
	a, b, c := txns[0], txns[1], txns[2]
	if a.Seq != 7 || a.Start != 1 || a.Commit != 5 || !a.Committed || !reflect.DeepEqual(a.Revokes, []uint64{50}) || len(a.Tags) != 2 {
		t.Errorf("fragment 7 = %+v; want start 1 commit 5, revoke 50, two tags", a)
	}
	if b.Seq != 8 || b.Start != 6 || b.Commit != 8 || !b.Committed || len(b.Tags) != 1 {
		t.Errorf("fragment 8 = %+v; want start 6 commit 8, one tag", b)
	}
	if c.Seq != 9 || c.Start != 10 || c.Committed || c.Commit != -1 || len(c.Tags) != 1 || !c.Stale {
		t.Errorf("fragment 9 = %+v; want an uncommitted stale fragment at 10 with one tag", c)
	}
	if !hasSub(c.Warnings, "fragment-end@") {
		t.Errorf("fragment 9 warnings %q; want one that names fragment-end@<block>", c.Warnings)
	}
}

func TestStaleFragmentStopsAtForeignHeader(t *testing.T) {
	// No checksum feature, so every copy that is there is "unverified". The second slot (journal block 3) is
	// overwritten by a later lap with a commit header of sequence 99.
	tx := liveTxn(7, 100, 101, 102)
	tx.Tags[1].Overwrite = jHeader(2, 99)
	img, _ := liveImage(t, 64, ext4test.JBD2{Sequence: 30, Start: 0, Txns: []ext4test.JTxn{tx}})
	txns := allTxns(t, img)
	if len(txns) != 3 {
		t.Fatalf("%d fragments; want 3 (7 up to the foreign header, the foreign header's own fragment, the orphan commit): %+v", len(txns), txns)
	}
	a := txns[0]
	if a.Seq != 7 || a.Start != 1 || a.Committed || a.Commit != -1 {
		t.Errorf("fragment 7 = %+v; want uncommitted from block 1", a)
	}
	if got, want := copies(a.Tags), []string{"unverified", "missing", "missing"}; !reflect.DeepEqual(got, want) {
		t.Errorf("copy states %v; want %v (the slot that holds a header and the later ones are missing)", got, want)
	}
	if len(a.Tags) == 3 && (a.Tags[1].JBlock != 3 || a.Tags[2].JBlock != 4 || a.Tags[1].FSBlock != 101) {
		t.Errorf("missing tags = %+v; want them to keep their block and slot", a.Tags[1:])
	}
	if b := txns[1]; b.Seq != 99 || b.Start != 3 || len(b.Tags) != 0 {
		t.Errorf("the foreign header is its own fragment: %+v; want seq 99 at block 3", b)
	}
	if c := txns[2]; c.Seq != 7 || c.Start != 5 || !c.Committed {
		t.Errorf("the orphan commit = %+v; want a committed fragment of 7 at block 5", c)
	}
}

func TestStaleFragmentStopsAtLiveRegion(t *testing.T) {
	// 16 blocks: a stale transaction of six tags at block 8 (descriptor 8, slots 9..14) is cut by the live
	// transaction that starts at block 14.
	stale := liveTxn(7, 100, 101, 102, 103, 104, 105)
	stale.At = 8
	live := liveTxn(20, 110)
	live.At = 14
	img, _ := liveImage(t, 16, ext4test.JBD2{Sequence: 20, Start: 14, Txns: []ext4test.JTxn{stale, live}})
	txns := allTxns(t, img)
	if len(txns) < 2 || txns[0].Stale || txns[0].Seq != 20 || !txns[0].Committed {
		t.Fatalf("txns %+v; want the live transaction 20 first", txns)
	}
	var s ext4.IndexTxn
	for _, tx := range txns {
		if tx.Stale && tx.Seq == 7 && tx.Start == 8 {
			s = tx
		}
	}
	if s.Seq != 7 {
		t.Fatalf("no stale fragment of 7 at block 8 in %+v", txns)
	}
	want := []string{"unverified", "unverified", "unverified", "unverified", "unverified", "missing"}
	if got := copies(s.Tags); !reflect.DeepEqual(got, want) || s.Committed {
		t.Errorf("stale fragment copy states %v committed %v; want %v uncommitted (the sixth slot is the live region)", got, s.Committed, want)
	}
	if c, _ := mustOpen(t, img).IndexConsumed(14); !c {
		t.Error("the live region's first block is not consumed")
	}
}

func TestStaleTagCopyStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		j    ext4test.JBD2
		want []string
	}{
		{"csum v3", ext4test.JBD2{CsumV3: true, Bit64: true}, []string{"verified", "mismatch"}},
		{"csum v2", ext4test.JBD2{CsumV2: true, Bit64: true}, []string{"verified", "mismatch"}},
		{"no checksum", ext4test.JBD2{Bit64: true}, []string{"unverified", "unverified"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := liveTxn(7, 100, 101)
			tx.Tags[1].Overwrite = []byte("a later lap wrote here")
			j := tc.j
			j.Sequence, j.Start, j.Txns = 30, 0, []ext4test.JTxn{tx}
			img, _ := liveImage(t, 64, j)
			txns := allTxns(t, img)
			if len(txns) != 1 || !txns[0].Stale || !txns[0].Committed {
				t.Fatalf("txns %+v; want one committed stale fragment", txns)
			}
			if got := copies(txns[0].Tags); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("copy states %v; want %v (an overwritten slot never verifies)", got, tc.want)
			}
			if txns[0].Csum == "bad" {
				t.Errorf("a stale mismatch made the fragment's checksum %q; the copy carries the state", txns[0].Csum)
			}
		})
	}
}

func TestStaleCommitOnly(t *testing.T) {
	img, l := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 30, Start: 0, Txns: []ext4test.JTxn{liveTxn(7, 100, 101)}})
	// Overwrite the descriptor (journal block 1): the data slots are not headers, the commit is.
	fs := l.JournalFSBlock(1)
	clear(img[fs*jtBS : (fs+1)*jtBS])
	txns := allTxns(t, img)
	if len(txns) != 1 {
		t.Fatalf("%d fragments; want the lone commit", len(txns))
	}
	c := txns[0]
	if c.Seq != 7 || c.Start != 4 || c.Commit != 4 || !c.Committed || len(c.Tags) != 0 || !c.Stale {
		t.Errorf("commit-only fragment = %+v; want seq 7 committed at 4 with no tags", c)
	}
	if !hasSub(c.Warnings, "the descriptor blocks of this transaction are overwritten") {
		t.Errorf("warnings %q; want the overwritten-descriptors warning", c.Warnings)
	}
	if info := jtWant(t, img); len(info.Warnings) != 0 || info.Incomplete {
		t.Errorf("Journal() = %+v; a commit with no descriptor is expected, not an anomaly (every mount restarts logging at s_first)", info)
	}
}

func TestStaleDuplicateSequences(t *testing.T) {
	a, b := liveTxn(7, 100), liveTxn(7, 101)
	a.At, b.At = 1, 10
	img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 30, Start: 0, Txns: []ext4test.JTxn{a, b}})
	var got []filesys.JournalTxn
	err := mustOpen(t, img).JournalTransactions(func(tx filesys.JournalTxn) bool { got = append(got, tx); return true })
	if err != nil || len(got) != 2 {
		t.Fatalf("%d transactions, %v; want two fragments of sequence 7", len(got), err)
	}
	if got[0].Seq != 7 || got[1].Seq != 7 || got[0].StartBlock != 1 || got[1].StartBlock != 10 || got[0].Blocks[0].FSBlock != 100 || got[1].Blocks[0].FSBlock != 101 {
		t.Errorf("fragments %+v; want two of sequence 7, told apart by StartBlock 1 and 10", got)
	}
}

func TestRevokeRule(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		revoke := func(seq uint32, noCommit bool, recs ...uint64) ext4test.JTxn {
			return ext4test.JTxn{Seq: seq, At: -1, Revokes: recs, NoCommit: noCommit}
		}
		img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 10, Start: 1, Txns: []ext4test.JTxn{
			liveTxn(10, 100, 101), // 100 revoked by 11; 101 only by the uncommitted 13
			revoke(11, false, 100),
			liveTxn(12, 100),         // newer than the revoke: not revoked
			revoke(13, true, 101, 7), // no commit: does not count
		}})
		txns := onlyLive(allTxns(t, img))
		if len(txns) != 4 {
			t.Fatalf("%d live txns; want 4", len(txns))
		}
		if !txns[0].Tags[0].Revoked || txns[0].Tags[1].Revoked {
			t.Errorf("txn 10 revoked flags %v %v; want true (committed revoke by 11) and false (uncommitted revoke by 13)", txns[0].Tags[0].Revoked, txns[0].Tags[1].Revoked)
		}
		if txns[2].Tags[0].Revoked {
			t.Error("txn 12's copy of block 100 is revoked by the older revoke of 11")
		}
	})
	t.Run("a stale revoke counts in either region", func(t *testing.T) {
		old := ext4test.JTxn{Seq: 25, At: 1, Revokes: []uint64{100}}
		img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 20, Start: 10, Txns: []ext4test.JTxn{
			old, {Seq: 20, At: 10, Tags: liveTxn(20, 100).Tags}, {Seq: 5, At: 20, Tags: liveTxn(5, 100, 100).Tags},
		}})
		var live, stale bool
		for _, tx := range allTxns(t, img) {
			if tx.Seq == 20 && len(tx.Tags) == 1 {
				live = tx.Tags[0].Revoked
			}
			if tx.Seq == 5 && len(tx.Tags) == 2 {
				stale = tx.Tags[0].Revoked && tx.Tags[1].Revoked
			}
		}
		if !live || !stale {
			t.Errorf("revoked: live tag %v, stale tags %v; want both (a committed stale revoke of 25 is >= 20 and >= 5)", live, stale)
		}
	})
	t.Run("an older stale revoke does not count", func(t *testing.T) {
		img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 30, Start: 0, Txns: []ext4test.JTxn{
			{Seq: 3, At: 1, Revokes: []uint64{100}}, {Seq: 9, At: 10, Tags: liveTxn(9, 100).Tags},
		}})
		seen := false
		for _, tx := range allTxns(t, img) {
			if tx.Seq == 9 {
				seen = true
				if tx.Tags[0].Revoked {
					t.Error("a copy logged by 9 is revoked by the older 3")
				}
			}
		}
		if !seen {
			t.Error("transaction 9 not found")
		}
	})
	t.Run("sequence wrap", func(t *testing.T) {
		img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 0xFFFFFFFF, Start: 1, Txns: []ext4test.JTxn{
			liveTxn(0xFFFFFFFF, 100), {Seq: 0, At: -1, Revokes: []uint64{100}},
		}})
		txns := onlyLive(allTxns(t, img))
		if len(txns) != 2 || !txns[0].Tags[0].Revoked {
			t.Errorf("txns %+v; want the copy logged by 0xFFFFFFFF revoked by the later sequence 0", txns)
		}
	})
	t.Run("Revoked lists the transaction's own records", func(t *testing.T) {
		img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{
			liveTxn(1, 100), {Seq: 2, At: -1, Revokes: []uint64{100, 1 << 33}},
		}})
		var revoked [][]uint64
		var flags []bool
		err := mustOpen(t, img).JournalTransactions(func(tx filesys.JournalTxn) bool {
			revoked = append(revoked, tx.Revoked)
			for _, b := range tx.Blocks {
				flags = append(flags, b.Revoked)
			}
			return true
		})
		if err != nil || len(revoked) != 2 || len(revoked[0]) != 0 || !reflect.DeepEqual(revoked[1], []uint64{100, 1 << 33}) || !reflect.DeepEqual(flags, []bool{true}) {
			t.Errorf("revoked %v flags %v err %v; want txn 2's records and the copy in txn 1 revoked", revoked, flags, err)
		}
	})
}

func TestStaleScanReadsEachBlockOnce(t *testing.T) {
	// Blocks 1..20 hold stale transactions; the live region runs from block 21 on. Every block is read once, by the
	// live scan, the stale pass or a fragment, never twice.
	var txns []ext4test.JTxn
	for i := range 4 {
		tx := liveTxn(uint32(5+i), 100+uint64(i), 110+uint64(i), 120+uint64(i)) // 5 blocks each: 1-5, 6-10, 11-15, 16-20
		tx.At = 1 + 5*i
		txns = append(txns, tx)
	}
	live := liveTxn(40, 130)
	live.At = 21
	txns = append(txns, live, liveTxn(41, 131), liveTxn(42, 132), liveTxn(43, 133))
	img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 40, Start: 21, Txns: txns})
	f := mustOpen(t, img)
	if all := txnsOf(t, f); len(all) != 8 {
		t.Fatalf("%d transactions; want 4 live and 4 stale", len(all))
	}
	builds, blocks := f.JournalStats()
	if builds != 1 || blocks > 64 {
		t.Errorf("builds %d, journal blocks read %d; want one build and at most 64 (each block once, the superblock included)", builds, blocks)
	}
}

func TestStalePassCapsAreIncomplete(t *testing.T) {
	img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 30, Start: 0, Txns: []ext4test.JTxn{
		liveTxn(5, 100), liveTxn(6, 101), liveTxn(7, 102), liveTxn(8, 103),
	}})
	f := mustOpen(t, img)
	f.SetJournalCaps(2, 1<<20, 1<<20)
	info, err := f.Journal()
	if err != nil || !info.Incomplete || !strings.HasPrefix(info.Stop, "cap-txns@") {
		t.Fatalf("Journal() = %+v, %v; want incomplete at cap-txns", info, err)
	}
	if txns := txnsOf(t, f); len(txns) != 2 {
		t.Errorf("%d fragments with a cap of 2", len(txns))
	}
}

// A cut image in the stale pass: the slot beyond the cut has no copy, and the result says it is incomplete.
func TestStaleImageEndIsIncomplete(t *testing.T) {
	img, l := liveImage(t, 16, ext4test.JBD2{Sequence: 30, Start: 0, Txns: []ext4test.JTxn{liveTxn(7, 100, 101)}})
	cut := l.JournalFSBlock(3) * jtBS // descriptor 1, slot 2 survive; slot 3 and the commit 4 are beyond the cut
	f, err := ext4.Open(bytes.NewReader(img[:cut]), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Journal()
	if err != nil || !info.Incomplete || info.Stop != "image-end@3" {
		t.Fatalf("Journal() = %+v, %v; want incomplete at image-end@3", info, err)
	}
	if !hasSub(info.Warnings, "journal blocks in all lie beyond") {
		t.Errorf("warnings %q; want the count of the blocks beyond the image", info.Warnings)
	}
	txns := txnsOf(t, f)
	if len(txns) != 1 || txns[0].Committed || !reflect.DeepEqual(copies(txns[0].Tags), []string{"unverified", "missing"}) {
		t.Errorf("txns %+v; want one uncommitted fragment with an unverified and a missing tag", txns)
	}
}
