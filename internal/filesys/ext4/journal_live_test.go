package ext4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// These tests build journals with ext4test (an independent encoder), set needs_recovery through PatchSuper where the
// journal has a live region, and read the index of the live scan back through the test accessors (3G-R3, R4).

func liveImage(t *testing.T, blocks int, j ext4test.JBD2) ([]byte, *ext4test.Layout) {
	t.Helper()
	img, l := jtImage(t, jtOpts(blocks), j)
	if j.Start != 0 {
		l.PatchSuper(img, func(sb []byte) { binary.LittleEndian.PutUint32(sb[0x60:], binary.LittleEndian.Uint32(sb[0x60:])|0x4) })
	}
	return img, l
}

func liveTxns(t *testing.T, img []byte) ([]ext4.IndexTxn, string) {
	t.Helper()
	f := mustOpen(t, img)
	txns, stop, err := f.IndexTxns()
	if err != nil {
		t.Fatalf("IndexTxns: %v", err)
	}
	return onlyLive(txns), stop
}

// onlyLive drops the stale fragments the stale pass (Task 6) adds after the live transactions.
func onlyLive(txns []ext4.IndexTxn) []ext4.IndexTxn {
	var out []ext4.IndexTxn
	for _, x := range txns {
		if !x.Stale {
			out = append(out, x)
		}
	}
	return out
}

func liveData(n byte) []byte { return bytes.Repeat([]byte{n}, jtBS) }

func liveTxn(seq uint32, fsblks ...uint64) ext4test.JTxn {
	t := ext4test.JTxn{Seq: seq, At: -1}
	for i, b := range fsblks {
		t.Tags = append(t.Tags, ext4test.JTag{FSBlock: b, Data: liveData(byte(seq)*16 + byte(i) + 1)})
	}
	return t
}

func fsBlocksOf(tags []ext4.IndexTag) []uint64 {
	var out []uint64
	for _, g := range tags {
		out = append(out, g.FSBlock)
	}
	return out
}

func TestLiveScanFollowsSequence(t *testing.T) {
	img, _ := liveImage(t, 64, ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 10, Start: 1,
		Txns: []ext4test.JTxn{liveTxn(10, 100, 101), liveTxn(11, 102, 103), liveTxn(12, 104, 105), liveTxn(20, 106, 107)},
	})
	txns, stop := liveTxns(t, img)
	if len(txns) != 3 {
		t.Fatalf("%d live transactions; want 3 (the fourth has the wrong sequence)", len(txns))
	}
	for i, tx := range txns {
		start, commit := int64(1+4*i), int64(4+4*i)
		if tx.Seq != uint32(10+i) || !tx.Committed || tx.Stale || tx.Start != start || tx.Commit != commit || tx.Csum != "ok" {
			t.Errorf("txn %d = %+v; want seq %d committed live, start %d commit %d csum ok", i, tx, 10+i, start, commit)
		}
		want := []uint64{uint64(100 + 2*i), uint64(101 + 2*i)}
		if got := fsBlocksOf(tx.Tags); !reflect.DeepEqual(got, want) {
			t.Errorf("txn %d tags %v; want %v", i, got, want)
		}
		if len(tx.Tags) == 2 && (tx.Tags[0].JBlock != uint32(start+1) || tx.Tags[1].JBlock != uint32(start+2)) {
			t.Errorf("txn %d data slots %d,%d; want %d,%d", i, tx.Tags[0].JBlock, tx.Tags[1].JBlock, start+1, start+2)
		}
		if len(tx.Tags) == 2 && (tx.Tags[0].Flags != 0 || tx.Tags[1].Flags != 0xA) {
			t.Errorf("txn %d flags %#x,%#x; want 0 (own UUID) and SAME_UUID|LAST (0xA)", i, tx.Tags[0].Flags, tx.Tags[1].Flags)
		}
	}
	if stop != "end-of-log@13" {
		t.Errorf("stop = %q; want end-of-log@13", stop)
	}
	f := mustOpen(t, img)
	for j := uint32(1); j <= 12; j++ {
		if c, err := f.IndexConsumed(j); err != nil || !c {
			t.Errorf("journal block %d consumed = %v, %v; want true", j, c, err)
		}
	}
	// Block 13 holds a header of sequence 20: not part of the live region, so a stale fragment starts there.
	if all, _, _ := f.IndexTxns(); len(all) != 4 || !all[3].Stale || all[3].Seq != 20 || all[3].Start != 13 {
		t.Errorf("transactions %+v; want the foreign-sequence transaction at 13 as a fourth, stale fragment", all)
	}
	info := jtWant(t, img)
	if info.Incomplete || info.Stop != "" || len(info.Warnings) != 0 || info.Start != 1 || !info.NeedsRecovery {
		t.Errorf("Journal() = %+v; want a complete, warning-free live journal", info)
	}
}

func TestLiveScanCleanJournalIsEmpty(t *testing.T) {
	img, _ := liveImage(t, 64, ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 12, Start: 0,
		Txns: []ext4test.JTxn{liveTxn(10, 100), liveTxn(11, 101)},
	})
	f := mustOpen(t, img)
	all, stop, err := f.IndexTxns()
	txns := onlyLive(all)
	if err != nil || len(txns) != 0 || stop != "" {
		t.Fatalf("a clean journal (s_start 0) indexes %d txns, stop %q, err %v; want none", len(txns), stop, err)
	}
	// What survives a clean unmount is stale (spec 9.1), never live.
	if len(all) != 2 || !all[0].Stale || !all[1].Stale {
		t.Errorf("a clean journal holding two transactions indexes %+v; want two stale fragments", all)
	}
}

func TestLiveScanStartNotAtATransaction(t *testing.T) {
	img, _ := liveImage(t, 64, ext4test.JBD2{Sequence: 5, Start: 9, Txns: []ext4test.JTxn{{Seq: 6, At: 9}}})
	txns, stop := liveTxns(t, img)
	if len(txns) != 0 || stop != "end-of-log@9" {
		t.Fatalf("s_start at a foreign sequence: %d txns, stop %q; want none and end-of-log@9", len(txns), stop)
	}
	info := jtWant(t, img)
	if !hasSub(info.Warnings, "s_start") {
		t.Errorf("warnings %q do not say that s_start holds no transaction", info.Warnings)
	}
}

func TestLiveScanWraps(t *testing.T) {
	// 16-block log: txn 5 at 14 (desc 14, slot 15, slot 1 after the wrap, commit 2), then txn 6 (desc 3, slot 4, commit 5).
	img, _ := liveImage(t, 16, ext4test.JBD2{
		Sequence: 5, Start: 14,
		Txns: []ext4test.JTxn{liveTxn(5, 100, 101), liveTxn(6, 102)},
	})
	txns, stop := liveTxns(t, img)
	if len(txns) != 2 {
		t.Fatalf("%d txns; want 2", len(txns))
	}
	if a := txns[0]; a.Start != 14 || a.Commit != 2 || !a.Committed || a.Tags[0].JBlock != 15 || a.Tags[1].JBlock != 1 {
		t.Errorf("wrapped txn = start %d commit %d slots %d,%d; want 14, 2, 15, 1", a.Start, a.Commit, a.Tags[0].JBlock, a.Tags[1].JBlock)
	}
	if b := txns[1]; b.Start != 3 || b.Commit != 5 || !b.Committed {
		t.Errorf("second txn = start %d commit %d; want 3, 5", b.Start, b.Commit)
	}
	if stop != "end-of-log@6" {
		t.Errorf("stop %q; want end-of-log@6", stop)
	}
}

func TestLiveScanUncommittedTail(t *testing.T) {
	last := liveTxn(3, 102)
	last.NoCommit = true
	img, _ := liveImage(t, 64, ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 1, Start: 1,
		Txns: []ext4test.JTxn{liveTxn(1, 100), liveTxn(2, 101), last},
	})
	txns, stop := liveTxns(t, img)
	if len(txns) != 3 {
		t.Fatalf("%d txns; want 3", len(txns))
	}
	if !txns[0].Committed || !txns[1].Committed {
		t.Errorf("the first two transactions are committed: %v %v", txns[0].Committed, txns[1].Committed)
	}
	if tl := txns[2]; tl.Committed || tl.Commit != -1 || len(tl.Tags) != 1 || tl.Start != 7 {
		t.Errorf("uncommitted tail = %+v; want Committed false, commit -1, one tag, start 7", tl)
	}
	if stop != "end-of-log@9" {
		t.Errorf("stop %q; want end-of-log@9", stop)
	}
}

func TestLiveScanBadDescriptorChecksumStops(t *testing.T) {
	bad := liveTxn(2, 101)
	bad.BadDescCsum = true
	img, _ := liveImage(t, 64, ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 1, Start: 1,
		Txns: []ext4test.JTxn{liveTxn(1, 100), bad, liveTxn(3, 102)},
	})
	txns, stop := liveTxns(t, img)
	if len(txns) != 2 {
		t.Fatalf("%d txns; want 2 (the region ends at the bad descriptor)", len(txns))
	}
	tb := txns[1]
	if tb.Committed || tb.Csum != "bad" || tb.Commit != -1 || len(tb.Tags) != 0 || !hasSub(tb.Warnings, "recovery may stop") {
		t.Errorf("bad-descriptor txn = %+v; want uncommitted, csum bad, no tags, a warning that the kernel's recovery may stop", tb)
	}
	if stop != "descriptor-checksum@4" {
		t.Errorf("stop %q; want descriptor-checksum@4", stop)
	}
	if info := jtWant(t, img); !hasSub(info.Warnings, "descriptor-checksum@4") {
		t.Errorf("Journal() warnings %q do not report the ended region", info.Warnings)
	}
}

func TestLiveScanBadCommitChecksumStops(t *testing.T) {
	bad := liveTxn(2, 101)
	bad.BadCommitCsum = true
	img, _ := liveImage(t, 64, ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 1, Start: 1,
		Txns: []ext4test.JTxn{liveTxn(1, 100), bad, liveTxn(3, 102)},
	})
	txns, stop := liveTxns(t, img)
	if len(txns) != 2 {
		t.Fatalf("%d txns; want 2", len(txns))
	}
	tb := txns[1]
	if tb.Committed || tb.Csum != "bad" || len(tb.Tags) != 1 || !hasSub(tb.Warnings, "recovery may stop") {
		t.Errorf("bad-commit txn = %+v; want uncommitted, csum bad, its tag kept, a warning", tb)
	}
	if stop != "commit-checksum@6" {
		t.Errorf("stop %q; want commit-checksum@6", stop)
	}
}

func TestLiveScanRevokeRecords(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bit64 bool
		recs  []uint64
	}{
		{"8-byte records", true, []uint64{5, 1<<33 | 7, 9}},
		{"4-byte records", false, []uint64{5, 6, 0xFFFFFFFF}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, _ := liveImage(t, 64, ext4test.JBD2{
				Bit64: tc.bit64, CsumV3: true, Sequence: 1, Start: 1,
				Txns: []ext4test.JTxn{{Seq: 1, At: -1, Revokes: tc.recs}, liveTxn(2, 100)},
			})
			txns, _ := liveTxns(t, img)
			if len(txns) != 2 || !reflect.DeepEqual(txns[0].Revokes, tc.recs) || !txns[0].Committed || txns[0].Commit != 2 {
				t.Fatalf("txns = %+v; want txn 1 with revokes %v committed at 2, then txn 2", txns, tc.recs)
			}
			if len(txns[1].Revokes) != 0 {
				t.Errorf("txn 2 revokes %v; want none", txns[1].Revokes)
			}
		})
	}
}

func TestLiveScanRevokeDamageEndsTheRegion(t *testing.T) {
	// A bad revoke tail checksum.
	bad := ext4test.JTxn{Seq: 1, At: -1, Revokes: []uint64{5}, BadRevokeCsum: true}
	img, _ := liveImage(t, 64, ext4test.JBD2{CsumV3: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{bad, liveTxn(2, 100)}})
	txns, stop := liveTxns(t, img)
	if len(txns) != 1 || txns[0].Committed || txns[0].Csum != "bad" || len(txns[0].Revokes) != 0 || stop != "revoke-checksum@1" {
		t.Errorf("bad revoke checksum: txns %+v stop %q; want one uncommitted txn with no records, revoke-checksum@1", txns, stop)
	}
	// r_count beyond the block, and below the header: structure damage (no checksum feature, so the count is read).
	for _, rc := range []uint32{jtBS + 4, 8, 17 + jtBS} {
		img, l := liveImage(t, 64, ext4test.JBD2{Sequence: 1, Start: 1, Txns: []ext4test.JTxn{{Seq: 1, At: -1, Revokes: []uint64{5}}, liveTxn(2, 100)}})
		fs := l.JournalFSBlock(1)
		jtBE32(img[fs*jtBS:], 0xC, rc)
		txns, stop := liveTxns(t, img)
		if len(txns) != 1 || txns[0].Committed || len(txns[0].Revokes) != 0 || stop != "revoke-damaged@1" {
			t.Errorf("r_count %d: txns %+v stop %q; want one uncommitted txn, revoke-damaged@1", rc, txns, stop)
		}
	}
}

func TestLiveScanUnexpectedBlockStops(t *testing.T) {
	img, l := liveImage(t, 64, ext4test.JBD2{Sequence: 1, Start: 1, Txns: []ext4test.JTxn{liveTxn(1, 100), liveTxn(2, 101)}})
	// Turn the second transaction's descriptor (journal block 4) into a journal-superblock-typed block of its sequence.
	fs := l.JournalFSBlock(4)
	jtBE32(img[fs*jtBS:], 4, 4)
	txns, stop := liveTxns(t, img)
	if len(txns) != 1 || stop != "unexpected-block@4" {
		t.Errorf("%d txns, stop %q; want 1 and unexpected-block@4", len(txns), stop)
	}
}

func TestLiveScanCycleEndsTheRegion(t *testing.T) {
	// A 5-block journal (blocks 1..4 usable): one transaction of four revoke blocks and no commit wraps onto block 1.
	var recs []uint64
	for i := range 4 * 252 {
		recs = append(recs, uint64(i+1))
	}
	img, _ := liveImage(t, 5, ext4test.JBD2{Sequence: 1, Start: 1, Txns: []ext4test.JTxn{{Seq: 1, At: -1, Revokes: recs, NoCommit: true}}})
	txns, stop := liveTxns(t, img)
	if len(txns) != 1 || txns[0].Committed || stop != "cycle@1" || len(txns[0].Revokes) != len(recs) {
		t.Fatalf("txns %+v stop %q; want one uncommitted txn with %d records and cycle@1", txns, stop, len(recs))
	}
	if info := jtWant(t, img); !hasSub(info.Warnings, "cycle@1") {
		t.Errorf("Journal() warnings %q do not report the cycle", info.Warnings)
	}
}

func TestLiveScanTagFormats(t *testing.T) {
	for _, tc := range []struct {
		name string
		j    ext4test.JBD2
	}{
		{"csum v3, 64bit: 16 bytes", ext4test.JBD2{CsumV3: true, Bit64: true}},
		{"csum v3, 32-bit: 16 bytes", ext4test.JBD2{CsumV3: true}},
		{"csum v2, 64bit: 14 bytes", ext4test.JBD2{CsumV2: true, Bit64: true}},
		{"csum v2, 32-bit: 10 bytes", ext4test.JBD2{CsumV2: true}},
		{"no checksum, 64bit: 12 bytes", ext4test.JBD2{Bit64: true}},
		{"no checksum, 32-bit: 8 bytes", ext4test.JBD2{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := tc.j
			j.Sequence, j.Start = 1, 1
			hi := uint64(0)
			if j.Bit64 {
				hi = 1 << 32
			}
			// three tags: the first carries the UUID, the second asks for its own, the third reuses the UUID.
			tx := ext4test.JTxn{Seq: 1, At: -1, Tags: []ext4test.JTag{
				{FSBlock: 0x10, Data: liveData(1)},
				{FSBlock: 0x11, Data: liveData(2), OwnUUID: true},
				{FSBlock: hi | 0x12, Data: liveData(3)},
			}}
			j.Txns = []ext4test.JTxn{tx, liveTxn(2, 0x20)}
			img, _ := liveImage(t, 64, j)
			txns, stop := liveTxns(t, img)
			if len(txns) != 2 || !txns[0].Committed || !txns[1].Committed {
				t.Fatalf("txns %+v stop %q; want two committed transactions (a wrong tag size loses the parse)", txns, stop)
			}
			if got, want := fsBlocksOf(txns[0].Tags), []uint64{0x10, 0x11, hi | 0x12}; !reflect.DeepEqual(got, want) {
				t.Errorf("tags %#x; want %#x", got, want)
			}
			g := txns[0].Tags
			if g[0].Flags != 0 || g[1].Flags != 0 || g[2].Flags != 0xA {
				t.Errorf("flags %#x %#x %#x; want 0 0 0xA", g[0].Flags, g[1].Flags, g[2].Flags)
			}
			wantCopy := "unverified"
			if j.CsumV2 || j.CsumV3 {
				wantCopy = "verified"
			}
			if g[0].Copy != wantCopy || g[1].Copy != wantCopy {
				t.Errorf("copy states %q %q; want %q", g[0].Copy, g[1].Copy, wantCopy)
			}
			if hi != 0 && g[2].Copy != "outside" {
				t.Errorf("a tag above 32 bits of a small filesystem has copy %q; want outside", g[2].Copy)
			}
		})
	}
}

func TestLiveScanTagChecksumStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		j    ext4test.JBD2
		want [2]string // the copy states of a good and a bad tag
	}{
		{"csum v3", ext4test.JBD2{CsumV3: true, Bit64: true}, [2]string{"verified", "mismatch"}},
		{"csum v2", ext4test.JBD2{CsumV2: true, Bit64: true}, [2]string{"verified", "mismatch"}},
		{"no checksum", ext4test.JBD2{Bit64: true}, [2]string{"unverified", "unverified"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := tc.j
			j.Sequence, j.Start = 1, 1
			j.Txns = []ext4test.JTxn{
				{Seq: 1, At: -1, Tags: []ext4test.JTag{{FSBlock: 100, Data: liveData(1)}, {FSBlock: 101, Data: liveData(2), BadCsum: true}}},
				liveTxn(2, 102),
			}
			img, _ := liveImage(t, 64, j)
			txns, stop := liveTxns(t, img)
			if len(txns) != 2 || !txns[0].Committed || !txns[1].Committed {
				t.Fatalf("txns %+v stop %q; a bad tag checksum must not end the scan (the kernel checks it at replay)", txns, stop)
			}
			g := txns[0].Tags
			if g[0].Copy != tc.want[0] || g[1].Copy != tc.want[1] {
				t.Errorf("copy states %q %q; want %v", g[0].Copy, g[1].Copy, tc.want)
			}
			if tc.want[1] == "mismatch" {
				if !hasSub(txns[0].Warnings, "recovery") || txns[0].Csum != "bad" {
					t.Errorf("mismatch txn: warnings %q csum %q; want a warning that the kernel's recovery would fail, csum bad", txns[0].Warnings, txns[0].Csum)
				}
			} else if len(txns[0].Warnings) != 0 {
				t.Errorf("unexpected warnings %q", txns[0].Warnings)
			}
			if txns[1].Tags[0].Copy != tc.want[0] {
				t.Errorf("a later good tag = %q; want %q", txns[1].Tags[0].Copy, tc.want[0])
			}
		})
	}
}

func TestLiveScanEscapeIsRecorded(t *testing.T) {
	magic := make([]byte, jtBS)
	binary.BigEndian.PutUint32(magic, ext4test.JBD2Magic)
	img, _ := liveImage(t, 64, ext4test.JBD2{
		CsumV3: true, Bit64: true, Sequence: 1, Start: 1,
		Txns: []ext4test.JTxn{{Seq: 1, At: -1, Tags: []ext4test.JTag{{FSBlock: 100, Data: magic}, {FSBlock: 101, Data: liveData(1)}}}},
	})
	txns, _ := liveTxns(t, img)
	if len(txns) != 1 || len(txns[0].Tags) != 2 {
		t.Fatalf("txns %+v; want one transaction with two tags", txns)
	}
	g := txns[0].Tags
	if !g[0].Escaped || g[0].Flags&ext4test.JTagEscape == 0 || g[1].Escaped {
		t.Errorf("escape flags: %+v %+v; want the first tag escaped only", g[0], g[1])
	}
	if g[0].Copy != "verified" {
		t.Errorf("an escaped copy is checksummed as logged: copy %q, want verified", g[0].Copy)
	}
}

func TestLiveScanTagOutsideFilesystem(t *testing.T) {
	img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1, Start: 1})
	nblocks := mustOpen(t, img).Info().Size / jtBS
	tx := ext4test.JTxn{Seq: 1, At: -1, Tags: []ext4test.JTag{
		{FSBlock: uint64(nblocks) - 1, Data: liveData(1)},
		{FSBlock: uint64(nblocks), Data: liveData(2)},
		{FSBlock: 1 << 40, Data: liveData(3)},
	}}
	img, _ = liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{tx}})
	txns, _ := liveTxns(t, img)
	if len(txns) != 1 {
		t.Fatalf("%d txns; want 1", len(txns))
	}
	g := txns[0].Tags
	if len(g) != 3 || g[0].Copy != "verified" || g[1].Copy != "outside" || g[2].Copy != "outside" {
		t.Fatalf("tags %+v; want the last block of the filesystem verified and the two beyond it outside", g)
	}
	if !hasSub(txns[0].Warnings, "outside") || !txns[0].Committed {
		t.Errorf("txn warnings %q committed %v; want an outside warning and a committed transaction", txns[0].Warnings, txns[0].Committed)
	}
	if g[2].FSBlock != 1<<40 {
		t.Errorf("tag block %d; want it kept as logged", g[2].FSBlock)
	}
}

func TestLiveScanCommitTime(t *testing.T) {
	tx := liveTxn(1, 100)
	tx.CommitSec, tx.CommitNsec = 1_700_000_123, 456_000_789
	img, _ := liveImage(t, 64, ext4test.JBD2{CsumV3: true, Bit64: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{tx, liveTxn(2, 101)}})
	txns, _ := liveTxns(t, img)
	if len(txns) != 2 {
		t.Fatalf("%d txns; want 2", len(txns))
	}
	want := filesys.Timestamp{T: time.Unix(1_700_000_123, 456_000_789).UTC(), ZoneKnown: true}
	if txns[0].CommitTime != want {
		t.Errorf("CommitTime = %+v; want %+v", txns[0].CommitTime, want)
	}
	if txns[1].CommitTime != (filesys.Timestamp{}) {
		t.Errorf("a commit with no time stamp has CommitTime %+v; want the zero value (unknown)", txns[1].CommitTime)
	}
}

func TestLiveScanCompatChecksumNotVerified(t *testing.T) {
	for _, tc := range []struct {
		name string
		j    ext4test.JBD2
		want string
		warn bool
	}{
		{"v1 crc32 commit checksum", ext4test.JBD2{CompatChecksum: true}, "not-verified", true},
		{"no checksum at all", ext4test.JBD2{}, "not-recorded", false},
		{"csum v3", ext4test.JBD2{CsumV3: true}, "ok", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := tc.j
			j.Sequence, j.Start, j.Txns = 1, 1, []ext4test.JTxn{liveTxn(1, 100)}
			img, _ := liveImage(t, 64, j)
			txns, _ := liveTxns(t, img)
			if len(txns) != 1 || txns[0].Csum != tc.want || !txns[0].Committed {
				t.Fatalf("txns %+v; want one committed txn with checksum %q", txns, tc.want)
			}
			if got := hasSub(txns[0].Warnings, "not verified"); got != tc.warn {
				t.Errorf("warnings %q; want a not-verified warning: %v", txns[0].Warnings, tc.warn)
			}
		})
	}
}

func TestLiveScanCapsMakeTheResultIncomplete(t *testing.T) {
	big := 1 << 20
	three := ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{
		liveTxn(1, 100, 101), {Seq: 2, At: -1, Revokes: []uint64{1, 2, 3}}, liveTxn(3, 102),
	}}
	for _, tc := range []struct {
		name               string
		txns, tags, revoke int
		stop               string
		wantTxns           int
	}{
		{"transactions", 2, big, big, "cap-txns@7", 2},
		{"tags", big, 1, big, "cap-tags@3", 1},
		{"revokes", big, big, 2, "cap-revokes@5", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, _ := liveImage(t, 64, three)
			f := mustOpen(t, img)
			f.SetJournalCaps(tc.txns, tc.tags, tc.revoke)
			info, err := f.Journal()
			if err != nil {
				t.Fatal(err)
			}
			if !info.Incomplete || info.Stop != tc.stop {
				t.Errorf("Incomplete %v Stop %q; want incomplete with %s", info.Incomplete, info.Stop, tc.stop)
			}
			txns, _, _ := f.IndexTxns()
			if len(txns) != tc.wantTxns {
				t.Errorf("%d txns indexed; want %d", len(txns), tc.wantTxns)
			}
			if tc.name == "tags" && len(txns) == 1 && (len(txns[0].Tags) != 1 || txns[0].Committed) {
				t.Errorf("a txn cut by the tag cap has %d tags, committed %v; want 1 tag and uncommitted", len(txns[0].Tags), txns[0].Committed)
			}
			if tc.name == "revokes" && len(txns) == 2 && (len(txns[1].Revokes) != 2 || txns[1].Committed) {
				t.Errorf("a txn cut by the revoke cap has %d records, committed %v; want 2 and uncommitted", len(txns[1].Revokes), txns[1].Committed)
			}
		})
	}
	// Without a cap nothing is cut.
	img, _ := liveImage(t, 64, three)
	if info := jtWant(t, img); info.Incomplete || info.Stop != "" {
		t.Errorf("an uncapped journal is incomplete: %+v", info)
	}
}

func TestJournalIndexBuiltOnce(t *testing.T) {
	img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{liveTxn(1, 100, 101), liveTxn(2, 102)}})
	f := mustOpen(t, img)
	for range 3 {
		if _, err := f.Journal(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.IndexTxns(); err != nil {
			t.Fatal(err)
		}
	}
	builds, blocks := f.JournalStats()
	if builds != 1 {
		t.Errorf("the journal was built %d times; want once", builds)
	}
	if blocks > 64+1 {
		t.Errorf("%d journal blocks read; want at most s_maxlen+1 = 65", blocks)
	}
	if blocks < 8 {
		t.Errorf("only %d journal blocks read; the live scan reads every block it consumes", blocks)
	}
}

func TestJournalIndexIOErrorNotCached(t *testing.T) {
	img, l := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{liveTxn(1, 100, 101), liveTxn(2, 102)}})
	fs := int64(l.JournalFSBlock(3)) // a data slot of the first transaction
	r := &jtOnceFailReader{Reader: bytes.NewReader(img), from: fs * jtBS, to: (fs + 1) * jtBS}
	f, err := ext4.Open(r, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	r.armed = true
	_, err = f.Journal()
	if r.failed != 1 {
		t.Fatalf("the injected failure fired %d times; want 1", r.failed)
	}
	if err == nil || errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, filesys.ErrUnsupported) || errors.Is(err, filesys.ErrLimit) {
		t.Fatalf("a failed journal block read = %v; want the I/O error itself", err)
	}
	txns, stop, err := f.IndexTxns()
	if err != nil || len(txns) != 2 || stop != "end-of-log@8" {
		t.Fatalf("after the read works again: %d txns, stop %q, err %v; the failure must not have been cached", len(txns), stop, err)
	}
	if builds, _ := f.JournalStats(); builds != 2 {
		t.Errorf("builds = %d; want 2 (the failed one and the retry)", builds)
	}
}

func TestJournalCapsReachTheScan(t *testing.T) {
	img, _ := liveImage(t, 64, ext4test.JBD2{Sequence: 1, Start: 1, Txns: []ext4test.JTxn{liveTxn(1, 100), liveTxn(2, 101), liveTxn(3, 102)}})
	f := mustOpen(t, img)
	f.SetJournalCaps(1, 1<<20, 1<<20)
	txns, _, err := f.IndexTxns()
	if err != nil || len(txns) != 1 {
		t.Fatalf("with a cap of 1 transaction: %d txns, %v; want 1", len(txns), err)
	}
	if info, _ := f.Journal(); !info.Incomplete || !strings.HasPrefix(info.Stop, "cap-txns@") {
		t.Errorf("Journal() = Incomplete %v Stop %q; want incomplete at cap-txns", info.Incomplete, info.Stop)
	}
}

// A damage stop (3G-R3, ruling C8) ends the live region as the oracle does, and also marks the result incomplete: a
// region that went unexamined must be visible in Info.Incomplete and Info.Stop, not only in a warning.
func TestLiveScanDamageStopsAreIncomplete(t *testing.T) {
	desc := liveTxn(2, 101)
	desc.BadDescCsum = true
	commit := liveTxn(2, 101)
	commit.BadCommitCsum = true
	for _, tc := range []struct {
		name string
		j    ext4test.JBD2
		patc func(img []byte, l *ext4test.Layout)
		stop string
		soft bool // the kernel only flags the condition from memory: the wording must not claim it stops
	}{
		{"descriptor checksum", ext4test.JBD2{Bit64: true, CsumV3: true, Txns: []ext4test.JTxn{liveTxn(1, 100), desc}}, nil, "descriptor-checksum@4", true},
		{"commit checksum", ext4test.JBD2{Bit64: true, CsumV3: true, Txns: []ext4test.JTxn{liveTxn(1, 100), commit}}, nil, "commit-checksum@6", true},
		{"revoke checksum", ext4test.JBD2{CsumV3: true, Txns: []ext4test.JTxn{{Seq: 1, At: -1, Revokes: []uint64{5}, BadRevokeCsum: true}}}, nil, "revoke-checksum@1", true},
		{
			"revoke r_count below the header",
			ext4test.JBD2{Txns: []ext4test.JTxn{{Seq: 1, At: -1, Revokes: []uint64{5}}}},
			func(img []byte, l *ext4test.Layout) { jtBE32(img[l.JournalFSBlock(1)*jtBS:], 0xC, 8) }, "revoke-damaged@1", true,
		},
		{
			"revoke r_count beyond the block",
			ext4test.JBD2{Txns: []ext4test.JTxn{{Seq: 1, At: -1, Revokes: []uint64{5}}}},
			func(img []byte, l *ext4test.Layout) { jtBE32(img[l.JournalFSBlock(1)*jtBS:], 0xC, jtBS+4) }, "revoke-damaged@1", false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := tc.j
			j.Sequence, j.Start = 1, 1
			img, l := liveImage(t, 64, j)
			if tc.patc != nil {
				tc.patc(img, l)
			}
			txns, stop := liveTxns(t, img)
			if stop != tc.stop {
				t.Fatalf("stop %q; want %q (a damage stop stays a stop, as in the oracle)", stop, tc.stop)
			}
			info := jtWant(t, img)
			if !info.Incomplete || info.Stop != tc.stop {
				t.Errorf("Journal() Incomplete %v Stop %q; want incomplete with %s", info.Incomplete, info.Stop, tc.stop)
			}
			if !hasSub(info.Warnings, tc.stop) {
				t.Errorf("warnings %q do not name %s", info.Warnings, tc.stop)
			}
			all := append(append([]string(nil), info.Warnings...), txns[len(txns)-1].Warnings...)
			if got := hasSub(all, "recovery would stop"); got == tc.soft {
				t.Errorf("warnings %q: firm \"recovery would stop\" wording present = %v; want %v", all, got, !tc.soft)
			}
			if tc.soft && !hasSub(all, "recovery may") {
				t.Errorf("warnings %q do not say the kernel's recovery may stop or fail", all)
			}
		})
	}
	// A clean end of the log is not damage.
	img, _ := liveImage(t, 64, ext4test.JBD2{Sequence: 1, Start: 1, Txns: []ext4test.JTxn{liveTxn(1, 100)}})
	if info := jtWant(t, img); info.Incomplete || info.Stop != "" {
		t.Errorf("a clean end of log is %+v; want complete", info)
	}
}

// A full lap of the log that ends cleanly at a block holding a header of an older sequence is the end of the log, not a
// cycle: the magic and sequence test comes before the consumed test (Task 5 review M2).
func TestLiveScanCleanWrapIsEndOfLogNotCycle(t *testing.T) {
	// Blocks 1..4 are all used: two transactions of a revoke block and a commit block each.
	img, _ := liveImage(t, 5, ext4test.JBD2{Sequence: 1, Start: 1, Txns: []ext4test.JTxn{
		{Seq: 1, At: -1, Revokes: []uint64{5}}, {Seq: 2, At: -1, Revokes: []uint64{6}},
	}})
	txns, stop := liveTxns(t, img)
	if len(txns) != 2 || !txns[0].Committed || !txns[1].Committed || stop != "end-of-log@1" {
		t.Fatalf("txns %+v stop %q; want two committed transactions and end-of-log@1", txns, stop)
	}
	info := jtWant(t, img)
	if info.Incomplete || info.Stop != "" || hasSub(info.Warnings, "cycle") {
		t.Errorf("Journal() = %+v; want a complete journal with no cycle warning", info)
	}
}

// A read past the end of a cut image is an incomplete stop image-end@<block>, not a cached corrupt verdict (ruling C9).
func TestLiveScanImageEndIsAnIncompleteStop(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cutAt    int // the first journal block that lies beyond the cut
		stop     string
		wantTxns int
	}{
		{"cut at a descriptor", 5, "image-end@5", 1},
		{"cut at a data slot", 6, "image-end@6", 2}, // the second transaction keeps its descriptor, without the tag
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Transaction 1: blocks 1-4 (descriptor, two slots, commit); transaction 2: blocks 5-7.
			img, l := liveImage(t, 16, ext4test.JBD2{Sequence: 1, Start: 1, Txns: []ext4test.JTxn{liveTxn(1, 100, 101), liveTxn(2, 102)}})
			cut := l.JournalFSBlock(tc.cutAt) * jtBS
			// The reader ends at the cut while the size given to Open does not: the extent map is accepted, and the
			// journal block read hits the end of the data.
			f, err := ext4.Open(bytes.NewReader(img[:cut]), int64(len(img)))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			info, err := f.Journal()
			if err != nil {
				t.Fatalf("Journal on a cut image = %v; want an incomplete result, not an error", err)
			}
			if !info.Incomplete || info.Stop != tc.stop || !hasSub(info.Warnings, tc.stop) {
				t.Errorf("Incomplete %v Stop %q warnings %q; want incomplete at %s with a warning", info.Incomplete, info.Stop, info.Warnings, tc.stop)
			}
			if tc.cutAt == 6 {
				lt := onlyLive(txnsOf(t, f))
				if len(lt) != 2 || !hasSub(lt[1].Warnings, "dropped") {
					t.Errorf("live txns %+v; want a warning on the cut transaction that its tags were dropped", lt)
				}
			}
			if txns, _, _ := f.IndexTxns(); len(onlyLive(txns)) != tc.wantTxns {
				t.Errorf("%d live txns; want %d", len(onlyLive(txns)), tc.wantTxns)
			}
			// The verdict is the same on a second call (it is built once, not an error to retry).
			if again, err := f.Journal(); err != nil || again.Stop != tc.stop {
				t.Errorf("second Journal() = %+v, %v", again, err)
			}
		})
	}
}
