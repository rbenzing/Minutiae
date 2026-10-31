package ext4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// Hostile journals (3G-R9, spec 14): a journal is attacker-controlled bytes. Every row below pins the exact typed
// error or the exact stop token, bounds the journal blocks read at twice s_maxlen, and checks that every tag names
// a block inside the filesystem or is flagged "outside".

// jhDeadline turns a hang (a missing visited set) into a failure instead of a stuck test binary.
func jhDeadline(t *testing.T, name string, fn func()) {
	t.Helper()
	limit := 20 * time.Second
	if d, ok := t.Deadline(); ok {
		if rest := time.Until(d) / 2; rest < limit {
			limit = rest
		}
	}
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s: the journal read did not finish within %v (an unbounded loop)", name, limit)
	}
}

type jhWant struct {
	corrupt      bool   // Journal() returns a *filesys.CorruptError
	limit        bool   // Journal() returns an error wrapping filesys.ErrLimit
	stop         string // JournalInfo.Stop (the exact stop token); "" when complete
	liveStop     string // why the live scan ended (the index stop)
	incomplete   bool
	txns         int // transactions in the index (live and stale); -1 = not pinned
	warning      string
	maxBlocks    int64   // journal blocks read, at most; 0 = 2*s_maxlen
	firstTxnTags int     // the tag count of the first transaction, 0 = not pinned
	caps         *[3]int // SetJournalCaps(txns, tags, revokes) before the read
}

func jhRow(t *testing.T, img []byte, w jhWant) {
	t.Helper()
	f := mustOpen(t, img)
	infoBefore := f.Info().Warnings
	if w.caps != nil {
		f.SetJournalCaps(w.caps[0], w.caps[1], w.caps[2])
	}
	var (
		info filesys.JournalInfo
		err  error
		txns []ext4.IndexTxn
		stop string
		terr error
	)
	jhDeadline(t, "row", func() {
		info, err = f.Journal()
		if err == nil {
			txns, stop, terr = f.IndexTxns()
		}
	})
	switch {
	case w.corrupt:
		var ce *filesys.CorruptError
		if !errors.As(err, &ce) {
			t.Fatalf("Journal() = %+v, %v; want a *filesys.CorruptError", info, err)
		}
		if errors.Is(err, filesys.ErrLimit) || errors.Is(err, filesys.ErrUnsupported) {
			t.Fatalf("Journal() error %v is also limit/unsupported", err)
		}
		return
	case w.limit:
		if !errors.Is(err, filesys.ErrLimit) {
			t.Fatalf("Journal() = %+v, %v; want an error wrapping ErrLimit", info, err)
		}
		return
	}
	if err != nil || terr != nil {
		t.Fatalf("Journal() = %v, IndexTxns = %v; want a bounded result", err, terr)
	}
	if info.Stop != w.stop || info.Incomplete != w.incomplete {
		t.Errorf("Stop %q Incomplete %v; want %q and %v", info.Stop, info.Incomplete, w.stop, w.incomplete)
	}
	if stop != w.liveStop {
		t.Errorf("live stop %q; want %q", stop, w.liveStop)
	}
	if w.txns >= 0 && len(txns) != w.txns {
		t.Errorf("%d transactions; want %d", len(txns), w.txns)
	}
	if w.firstTxnTags != 0 && (len(txns) == 0 || len(txns[0].Tags) != w.firstTxnTags) {
		t.Errorf("the first transaction has the wrong number of tags; want %d", w.firstTxnTags)
	}
	if w.warning != "" && !hasSub(info.Warnings, w.warning) {
		var all []string
		for _, x := range txns {
			all = append(all, x.Warnings...)
		}
		if !hasSub(all, w.warning) {
			t.Errorf("no warning contains %q; journal %q, transactions %q", w.warning, info.Warnings, all)
		}
	}
	maxBlocks := w.maxBlocks
	if maxBlocks == 0 {
		maxBlocks = 2 * info.Blocks
	}
	if _, read := f.JournalStats(); read > maxBlocks {
		t.Errorf("%d journal blocks read; want at most %d", read, maxBlocks)
	}
	fsBlocks := uint64(len(img) / jtBS)
	for _, x := range txns {
		for _, g := range x.Tags {
			if g.FSBlock >= fsBlocks && g.Copy != "outside" {
				t.Errorf("tag %d of txn %d is beyond the %d-block filesystem but its copy is %q, not outside", g.FSBlock, x.Seq, fsBlocks, g.Copy)
			}
		}
	}
	if after := f.Info().Warnings; len(after) != len(infoBefore) {
		t.Errorf("the journal added filesystem warnings: %q", after)
	}
}

func jhSuper(img []byte, l *ext4test.Layout, edit func(sb []byte)) {
	jtPatchSuper(img, l, edit)
}

func TestJBD2Hostile(t *testing.T) {
	two := func() ext4test.JBD2 {
		return ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{liveTxn(1, 100, 101), liveTxn(2, 102)}}
	}
	rows := []jhSpec{
		{"s_start == s_maxlen", func(t *testing.T) []byte {
			img, l := liveImage(t, 64, two())
			jhSuper(img, l, func(sb []byte) { jtBE32(sb, 0x1C, 64) })
			return img
		}, jhWant{corrupt: true}},
		{"s_start 0xFFFFFFFF", func(t *testing.T) []byte {
			img, l := liveImage(t, 64, two())
			jhSuper(img, l, func(sb []byte) { jtBE32(sb, 0x1C, 0xFFFFFFFF) })
			return img
		}, jhWant{corrupt: true}},
		{"s_start below s_first", func(t *testing.T) []byte {
			img, l := liveImage(t, 64, two())
			jhSuper(img, l, func(sb []byte) { jtBE32(sb, 0x14, 5); jtBE32(sb, 0x1C, 3) })
			return img
		}, jhWant{corrupt: true}},
		{"s_maxlen zero", func(t *testing.T) []byte {
			img, l := liveImage(t, 64, two())
			jhSuper(img, l, func(sb []byte) { jtBE32(sb, 0x10, 0) })
			return img
		}, jhWant{corrupt: true}},
		{"s_maxlen beyond the inode", func(t *testing.T) []byte {
			img, l := liveImage(t, 64, two())
			jhSuper(img, l, func(sb []byte) { jtBE32(sb, 0x10, 0xFFFFFFFF) })
			return img
		}, jhWant{corrupt: true}},
		{"s_first zero", func(t *testing.T) []byte {
			img, l := liveImage(t, 64, two())
			jhSuper(img, l, func(sb []byte) { jtBE32(sb, 0x14, 0) })
			return img
		}, jhWant{corrupt: true}},
		{"descriptor without LAST_TAG", jhNoLastTag, jhWant{liveStop: "end-of-log@39", txns: 1, firstTxnTags: 37}},
		{"descriptor without LAST_TAG, checksummed", jhNoLastTagCsum, jhWant{stop: "descriptor-checksum@1", liveStop: "descriptor-checksum@1", incomplete: true, txns: 3}},
	}
	rows = append(rows, jhMoreRows()...)
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			jhRow(t, tc.img(t), tc.want)
		})
	}
}

// jhNoLastTag: one descriptor of two tags in a journal without checksums (12-byte tags: the first at 12 with the
// journal UUID after it, the second at 40, its 16-bit flags at +6); the LAST_TAG flag (8) of the second tag is
// cleared, so the tag list runs to the end of the block.
func jhNoLastTag(t *testing.T) []byte {
	img, l := liveImage(t, 64, ext4test.JBD2{Bit64: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{liveTxn(1, 100, 101), liveTxn(2, 102)}})
	fs := l.JournalFSBlock(1)
	b := img[fs*jtBS:]
	if fl := binary.BigEndian.Uint16(b[40+6:]); fl&8 == 0 {
		t.Fatalf("the second tag has flags %#x; the row is mispositioned", fl)
	}
	binary.BigEndian.PutUint16(b[40+6:], binary.BigEndian.Uint16(b[40+6:])&^8)
	return img
}

// jhNoLastTagCsum is the same edit on a checksummed journal: the descriptor checksum no longer matches.
func jhNoLastTagCsum(t *testing.T) []byte {
	img, l := liveImage(t, 64, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{liveTxn(1, 100, 101), liveTxn(2, 102)}})
	fs := l.JournalFSBlock(1)
	b := img[fs*jtBS:]
	if fl := binary.BigEndian.Uint32(b[44+4:]); fl&8 == 0 {
		t.Fatalf("the second tag has flags %#x; the row is mispositioned", fl)
	}
	jtBE32(b, 44+4, jtBE(b, 44+4)&^8)
	return img
}

type jhSpec struct {
	name string
	img  func(t *testing.T) []byte
	want jhWant
}

// jhRevokeCount builds a journal whose first transaction is a revoke block (journal block 1) followed by its commit,
// with r_count (the used bytes of the revoke block, header included) replaced by rc. No checksums, so the damage is
// the count and nothing else.
func jhRevokeCount(rc uint32) func(t *testing.T) []byte {
	return func(t *testing.T) []byte {
		img, l := liveImage(t, 64, ext4test.JBD2{Bit64: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{
			{Seq: 1, At: -1, Revokes: []uint64{7, 8, 9}},
		}})
		b := img[l.JournalFSBlock(1)*jtBS:]
		if typ := jtBE(b, 4); typ != 5 {
			t.Fatalf("journal block 1 has type %d; want a revoke block (5)", typ)
		}
		jtBE32(b, 12, rc)
		return img
	}
}

func jhMoreRows() []jhSpec {
	big := 1 << 20
	return []jhSpec{
		{"r_count zero", jhRevokeCount(0), jhWant{stop: "revoke-damaged@1", liveStop: "revoke-damaged@1", incomplete: true, txns: 2, warning: "r_count 0"}},
		{"r_count below the header", jhRevokeCount(15), jhWant{stop: "revoke-damaged@1", liveStop: "revoke-damaged@1", incomplete: true, txns: 2, warning: "r_count 15"}},
		{"r_count beyond the block", jhRevokeCount(0xFFFFFFFF), jhWant{stop: "revoke-damaged@1", liveStop: "revoke-damaged@1", incomplete: true, txns: 2, warning: "r_count 4294967295"}},
		{"r_count one past the block", jhRevokeCount(jtBS + 1), jhWant{stop: "revoke-damaged@1", liveStop: "revoke-damaged@1", incomplete: true, txns: 2, warning: "r_count 1025"}},
		{"a live chain wrapping back to s_start", func(t *testing.T) []byte {
			// A 5-block journal (blocks 1..4 usable): one transaction of four revoke blocks and no commit wraps onto block 1.
			var recs []uint64
			for i := range 4 * 252 {
				recs = append(recs, uint64(i+1))
			}
			img, _ := liveImage(t, 5, ext4test.JBD2{Sequence: 1, Start: 1, Txns: []ext4test.JTxn{{Seq: 1, At: -1, Revokes: recs, NoCommit: true}}})
			return img
		}, jhWant{liveStop: "cycle@1", txns: 1, warning: "reaches journal block 1 again"}},
		{"a tag naming block 2^63", func(t *testing.T) []byte {
			tx := ext4test.JTxn{Seq: 1, At: -1, Tags: []ext4test.JTag{
				{FSBlock: 1 << 63, Data: liveData(1)}, {FSBlock: 1<<64 - 1, Data: liveData(2)}, {FSBlock: 100, Data: liveData(3)},
			}}
			img, _ := liveImage(t, 64, ext4test.JBD2{Bit64: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{tx}})
			return img
		}, jhWant{liveStop: "end-of-log@6", txns: 1, firstTxnTags: 3, warning: "outside the"}},
		{"a commit at every block", func(t *testing.T) []byte {
			var txns []ext4test.JTxn
			for i := 1; i < 64; i++ {
				txns = append(txns, ext4test.JTxn{Seq: uint32(i), At: i})
			}
			img, _ := liveImage(t, 64, ext4test.JBD2{Sequence: 1, Start: 1, Txns: txns})
			return img
		}, jhWant{liveStop: "end-of-log@1", txns: 63}},
		{"a stale pass over headers of one sequence", func(t *testing.T) []byte {
			var txns []ext4test.JTxn
			for i := 1; i < 64; i++ {
				txns = append(txns, ext4test.JTxn{Seq: 7, At: i})
			}
			img, _ := liveImage(t, 64, ext4test.JBD2{Sequence: 7, Txns: txns})
			return img
		}, jhWant{txns: 63}},
		{"a journal inode with a hole", func(t *testing.T) []byte {
			img, l := liveImage(t, 64, ext4test.JBD2{Sequence: 1, Start: 1, Txns: []ext4test.JTxn{liveTxn(1, 100)}})
			// Replace the single extent of inode 8 by two, with logical blocks 10..19 missing.
			l.PatchInode(img, 8, func(raw []byte) {
				ib := raw[0x28:]
				le := binary.LittleEndian
				if le.Uint16(ib) != 0xF30A || le.Uint16(ib[2:]) != 1 || le.Uint16(ib[6:]) != 0 {
					t.Fatalf("the journal inode is not a one-extent depth-0 tree")
				}
				first := le.Uint32(ib[12+8:])
				n := le.Uint16(ib[12+4:])
				le.PutUint16(ib[2:], 2)
				le.PutUint16(ib[12+4:], 10)
				le.PutUint32(ib[24:], 20)
				le.PutUint16(ib[24+4:], n-10)
				le.PutUint16(ib[24+6:], 0)
				le.PutUint32(ib[24+8:], first+10)
			})
			return img
		}, jhWant{corrupt: true}},
		{"1500 one-block transactions under a cap of 1000", func(t *testing.T) []byte {
			var txns []ext4test.JTxn
			for i := 1; i <= 1500; i++ {
				txns = append(txns, ext4test.JTxn{Seq: uint32(i), At: i})
			}
			img, _ := liveImage(t, 1600, ext4test.JBD2{Sequence: 1, Start: 1, Txns: txns})
			return img
		}, jhWant{stop: "cap-txns@1001", liveStop: "cap-txns@1001", incomplete: true, txns: 1000, caps: &[3]int{1000, big, big}, warning: "stale pass skipped"}},
	}
}

func TestJBD2CapsAreIncomplete(t *testing.T) {
	// The documented caps (3G-R9), through the exported constants the reader uses.
	if ext4.MaxJournalBytes != 4<<30 || ext4.MaxJournalTxns != 1_000_000 || ext4.MaxJournalTags != 1<<21 || ext4.MaxJournalRevokes != 1<<21 {
		t.Errorf("caps = %d, %d, %d, %d; want 4 GiB, 1000000, 2^21, 2^21", int64(ext4.MaxJournalBytes), ext4.MaxJournalTxns, ext4.MaxJournalTags, ext4.MaxJournalRevokes)
	}
	big := 1 << 20
	three := ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{
		liveTxn(1, 100, 101), {Seq: 2, At: -1, Revokes: []uint64{1, 2, 3}}, liveTxn(3, 102),
	}}
	for _, tc := range []struct {
		name               string
		txns, tags, revoke int
		stop               string
		committed          int // transactions of the result that are committed
	}{
		{"transactions", 2, big, big, "cap-txns@7", 2},
		{"tags", big, 1, big, "cap-tags@3", 0},
		{"revokes", big, big, 2, "cap-revokes@5", 1},
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
			if !hasSub(info.Warnings, "cap") {
				t.Errorf("no warning names the cap: %q", info.Warnings)
			}
			committed := 0
			if err := f.JournalTransactions(func(tx filesys.JournalTxn) bool {
				if tx.Committed {
					committed++
				}
				return true
			}); err != nil {
				t.Fatal(err)
			}
			if committed != tc.committed {
				t.Errorf("%d committed transactions after the %s cap; want %d (the cut transaction is not committed)", committed, tc.name, tc.committed)
			}
		})
	}
}

// jhAlloc returns the bytes allocated (runtime.MemStats.TotalAlloc) and the allocation count of opening img and
// reading its journal in full.
func jhAlloc(t *testing.T, img []byte) (nbytes uint64, allocs uint64) {
	t.Helper()
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Journal()
	_ = f.JournalTransactions(func(filesys.JournalTxn) bool { return true })
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc, b.Mallocs - a.Mallocs
}

func TestJBD2AllocationIndependentOfDeclaredCounts(t *testing.T) {
	base := ext4test.JBD2{Bit64: true, Sequence: 1, Start: 1, Txns: []ext4test.JTxn{liveTxn(1, 100, 101), {Seq: 2, At: -1, Revokes: []uint64{7, 8}}}}
	benign, _ := liveImage(t, 64, base)
	jhAlloc(t, benign) // warm up
	baseBytes, baseAllocs := jhAlloc(t, benign)

	fc := base
	rev := base
	rev.Txns = []ext4test.JTxn{{Seq: 1, At: -1, Revokes: []uint64{7, 8}}, liveTxn(2, 100)}
	fc.FastCommit = true
	rows := []struct {
		name string
		jbd  *ext4test.JBD2
		edit func(img []byte, l *ext4test.Layout)
	}{
		{"s_maxlen 2^32-1", nil, func(img []byte, l *ext4test.Layout) {
			jtPatchSuper(img, l, func(sb []byte) { jtBE32(sb, 0x10, 0xFFFFFFFF) })
		}},
		{"s_num_fc_blks 2^32-1 (fast commit on)", &fc, func(img []byte, l *ext4test.Layout) {
			jtPatchSuper(img, l, func(sb []byte) { jtBE32(sb, 0x54, 0xFFFFFFFF) })
		}},
		{"r_count 2^32-1", &rev, func(img []byte, l *ext4test.Layout) { jtBE32(img[l.JournalFSBlock(1)*jtBS:], 12, 0xFFFFFFFF) }},
		{"a descriptor claiming 2^32-1 tags", nil, func(img []byte, l *ext4test.Layout) {
			b := img[l.JournalFSBlock(1)*jtBS:]
			for i := 12; i < jtBS; i++ { // every flags word: no LAST_TAG, SAME_UUID; blocks 2^32-1
				b[i] = 0xFF
			}
		}},
	}
	// The slack covers the slices that scale with the journal (a 64-block journal), not with any declared count.
	const slackBytes, slackAllocs = 256 << 10, 400
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			j := base
			if tc.jbd != nil {
				j = *tc.jbd
			}
			img, l := liveImage(t, 64, j)
			tc.edit(img, l)
			jhAlloc(t, img)
			bytes, allocs := jhAlloc(t, img)
			t.Logf("bytes %d (benign %d), allocs %d (benign %d)", bytes, baseBytes, allocs, baseAllocs)
			if bytes > baseBytes+slackBytes {
				t.Errorf("%d bytes allocated; the benign journal needs %d: the allocation follows a declared count", bytes, baseBytes)
			}
			if allocs > baseAllocs+slackAllocs {
				t.Errorf("%d allocations; the benign journal needs %d", allocs, baseAllocs)
			}
		})
	}
}
