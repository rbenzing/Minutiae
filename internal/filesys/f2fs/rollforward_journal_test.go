package f2fs_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// The Journaler view of the chain (3H-R9).

func TestJournalInfoOfChain(t *testing.T) {
	fx := newRFFix(t)
	f := fx.open(t)
	j, err := f.Journal()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := rfWalk(t, f)
	if j.Type != "f2fs-rollforward" || j.BlockSize != 4096 || j.Blocks != 5 || j.Sequence != uint32(c.CheckpointVersion) || j.Start != fx.tree.ChainAddrs[0] ||
		!slices.Equal(j.Features, []string{"crc-recovery"}) || !j.NeedsRecovery || len(j.Warnings) != 0 || j.Version != 0 {
		t.Errorf("Journal = %+v", j)
	}

	// NOCRC wins over CRC; an empty chain is empty.
	o := f2fstest.Options{NodeSegs: 2, Unclean: true, CPFlags: f2fstest.CPCRCRecovery | f2fstest.CPNoCRCRecovery}
	img, _ := f2fstest.BuildTree(o, []f2fstest.File{{Path: "/p.bin", Data: rfStamp(1), PostCheckpoint: true}})
	nf, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if j, err := nf.Journal(); err != nil || !slices.Equal(j.Features, []string{"nocrc-recovery"}) || !j.NeedsRecovery {
		t.Errorf("nocrc Journal = %+v, %v", j, err)
	}
	o = f2fstest.Options{NodeSegs: 2, Unclean: true}
	img, _ = f2fstest.BuildTree(o, nil)
	ef, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	j, err = ef.Journal()
	visits := 0
	terr := ef.JournalTransactions(func(filesys.JournalTxn) bool { visits++; return true })
	if err != nil || terr != nil || j.Blocks != 0 || j.NeedsRecovery || visits != 0 || len(j.Features) != 0 {
		t.Errorf("empty chain: Journal = %+v, %v; %d transactions, %v", j, err, visits, terr)
	}
}

func TestJournalNothingToReplayDoesNotNeedRecovery(t *testing.T) {
	o := f2fstest.Options{NodeSegs: 2, Unclean: true, CPFlags: f2fstest.CPCRCRecovery}
	o.Chain = []f2fstest.ChainNode{{Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: 90, Mode: 0o100644, Links: 1, PIno: f2fstest.RootIno, Name: "g"}), Fsync: true}}
	img, _ := f2fstest.BuildTree(o, nil)
	f, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	j, err := f.Journal()
	var tx []filesys.JournalTxn
	terr := f.JournalTransactions(func(t filesys.JournalTxn) bool { tx = append(tx, t); return true })
	if err != nil || terr != nil || j.Blocks != 1 || j.NeedsRecovery || len(tx) != 1 || tx[0].Committed || len(tx[0].Blocks) != 1 {
		t.Errorf("Journal = %+v, %v; txns %+v, %v; want one uncommitted transaction", j, err, tx, terr)
	}
}

func TestJournalOneTransaction(t *testing.T) {
	fx := newRFFix(t)
	f := fx.open(t)
	c, nodes := rfWalk(t, f)
	var txns []filesys.JournalTxn
	if err := f.JournalTransactions(func(tx filesys.JournalTxn) bool { txns = append(txns, tx); return true }); err != nil {
		t.Fatal(err)
	}
	if len(txns) != 1 {
		t.Fatalf("%d transactions, want 1", len(txns))
	}
	tx := txns[0]
	if tx.Seq != uint32(c.CheckpointVersion) || !tx.Committed || tx.Region != "live" || len(tx.Blocks) != 5 || len(tx.Revoked) != 0 {
		t.Errorf("txn = %+v", tx)
	}
	for i, n := range nodes {
		if tx.Blocks[i] != (filesys.JournalTag{FSBlock: n.Block, Flags: n.Flag}) {
			t.Errorf("tag %d = %+v, want block %d flags %#x", i, tx.Blocks[i], n.Block, n.Flag)
		}
	}
	// Mutating the answer changes nothing later; an early stop is not an error.
	tx.Blocks[0].FSBlock = 1
	calls := 0
	var again filesys.JournalTxn
	if err := f.JournalTransactions(func(t filesys.JournalTxn) bool { calls++; again = t; return false }); err != nil || calls != 1 || again.Blocks[0].FSBlock != nodes[0].Block {
		t.Errorf("second call: %d calls, tag %+v, err %v", calls, again.Blocks[0], err)
	}
}

type countReaderAt struct {
	r *bytes.Reader
	n atomic.Int64
}

func (c *countReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.n.Add(1)
	return c.r.ReadAt(p, off)
}

var _ io.ReaderAt = (*countReaderAt)(nil)

func TestJournalBlockReresolves(t *testing.T) {
	fx := newRFFix(t)
	cr := &countReaderAt{r: bytes.NewReader(fx.img)}
	f, err := f2fs.Open(cr, int64(len(fx.img)))
	if err != nil {
		t.Fatal(err)
	}
	var tx filesys.JournalTxn
	if err := f.JournalTransactions(func(t filesys.JournalTxn) bool { tx = t; return true }); err != nil {
		t.Fatal(err)
	}
	for i, a := range fx.tree.ChainAddrs {
		b, run, err := f.JournalBlock(tx, i)
		if err != nil || !bytes.Equal(b, fx.img[int(a)*bs:(int(a)+1)*bs]) || run != (filesys.Run{Offset: int64(a) * bs, Length: bs}) {
			t.Fatalf("JournalBlock(%d) = %d bytes, %+v, %v; want block %d", i, len(b), run, err, a)
		}
	}

	// A transaction of another image: same sequence, different tags.
	oimg, _ := f2fstest.BuildTree(fx.o, []f2fstest.File{{Path: "/p.bin", Data: rfStamp(1), PostCheckpoint: true}})
	other, err := f2fs.Open(bytes.NewReader(oimg), int64(len(oimg)))
	if err != nil {
		t.Fatal(err)
	}

	forgedSeq := tx
	forgedSeq.Seq++
	forgedTag := tx
	forgedTag.Blocks = slices.Clone(tx.Blocks)
	forgedTag.Blocks[1].FSBlock++
	forgedFlags := tx
	forgedFlags.Blocks = slices.Clone(tx.Blocks)
	forgedFlags.Blocks[1].Flags ^= 1
	short := tx
	short.Blocks = tx.Blocks[:1]
	cases := []struct {
		name string
		fs   *f2fs.FS
		tx   filesys.JournalTxn
		i    int
	}{
		{"forged seq", f, forgedSeq, 1},
		{"forged block", f, forgedTag, 1},
		{"forged flags", f, forgedFlags, 1},
		{"index out of range", f, tx, 5},
		{"negative index", f, tx, -1},
		{"index past the transaction's tags", f, short, 1},
		{"tag from another image", other, tx, 1},
	}
	before := cr.n.Load()
	for _, tc := range cases {
		b, run, err := tc.fs.JournalBlock(tc.tx, tc.i)
		if !errors.Is(err, filesys.ErrNotFound) || b != nil || run != (filesys.Run{}) {
			t.Errorf("%s: = %d bytes, %+v, %v; want ErrNotFound", tc.name, len(b), run, err)
		}
	}
	if after := cr.n.Load(); after != before {
		t.Errorf("forged requests read the image %d times", after-before)
	}
}
