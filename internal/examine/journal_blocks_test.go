package examine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// jrnBlocks copies the chain blocks selected by o.
func jrnBlocks(t *testing.T, s *examine.Session, o examine.JournalBlocksOptions) examine.JournalBlocksSummary {
	t.Helper()
	sum, err := s.JournalBlocks(context.Background(), o)
	if err != nil {
		t.Fatalf("JournalBlocks: %v", err)
	}
	return sum
}

// jrnSeq is the transaction sequence of the chain of s.
func jrnSeq(t *testing.T, s *examine.Session) uint32 {
	t.Helper()
	chain, _ := jrnChain(t, s)
	return uint32(chain.CheckpointVersion)
}

func TestJournalBlocksCopiesEveryChainBlock(t *testing.T) {
	s, disk, _ := jrnSession(t)
	chain, nodes := jrnChain(t, s)
	part, err := s.Partition(-1)
	if err != nil {
		t.Fatal(err)
	}
	seq := jrnSeq(t, s)
	sum := jrnBlocks(t, s, examine.JournalBlocksOptions{JournalOptions: jo, Seq: seq})
	if len(sum.Artifacts) != chain.Nodes || sum.Copied != chain.Nodes || sum.NotFree != 0 || sum.Files != chain.Nodes {
		t.Fatalf("%d artifacts, copied %d, not-free %d for %d nodes", len(sum.Artifacts), sum.Copied, sum.NotFree, chain.Nodes)
	}
	for i, n := range nodes {
		rec := sum.Artifacts[i]
		rv, d := recoveryOf(rec), rec.Source.Derived
		off := part.Start + int64(n.Block)*bs
		if rec.Source.Kind != evidence.KindJournal || rv.Class != evidence.ClassJournalBlock || rv.Method != "f2fs-rollforward" ||
			rv.Confidence != nil || rv.Scope != "unallocated" || rv.Alloc.Free != bs || rec.Incomplete {
			t.Errorf("node %d: kind %q recovery %+v incomplete %v", i, rec.Source.Kind, rv, rec.Incomplete)
		}
		if rv.Journal == nil || rv.Journal.Seq != seq || rv.Journal.Committed != n.WouldReplay || rv.Journal.Region != "live" {
			t.Errorf("node %d: journal ref %+v, node %+v", i, rv.Journal, n)
		}
		wantBasis := []string{
			fmt.Sprintf("rollforward:seq=%d", seq), fmt.Sprintf("chain-index=%d", n.Index),
			fmt.Sprintf("nid=%d", n.NID), fmt.Sprintf("ino=%d", n.Ino), fmt.Sprintf("blk=%d", n.Block),
		}
		if strings.Join(rv.Basis, ",") != strings.Join(wantBasis, ",") {
			t.Errorf("node %d: basis %v, want %v", i, rv.Basis, wantBasis)
		}
		if len(d.Runs) != 1 || d.Runs[0] != (evidence.Run{Offset: off, Length: bs}) {
			t.Errorf("node %d: runs %v, want [{%d %d}]", i, d.Runs, off, bs)
		}
		if !strings.HasSuffix(rec.Path, fmt.Sprintf("journal/p1-f2fs/seq%d-fsblk%d.bin", seq, n.Block)) {
			t.Errorf("node %d: path %q", i, rec.Path)
		}
		if got := readArtifact(t, s.Case, rec); !bytes.Equal(got, disk[off:off+bs]) {
			t.Errorf("node %d: the copy differs from the image block", i)
		}
	}
	verifyOK(t, s.Case)
}

func TestJournalBlocksFSBlockFilter(t *testing.T) {
	s, _, _ := jrnSession(t)
	_, nodes := jrnChain(t, s)
	seq := jrnSeq(t, s)
	blk := nodes[1].Block
	sum := jrnBlocks(t, s, examine.JournalBlocksOptions{JournalOptions: jo, Seq: seq, FSBlock: &blk})
	if len(sum.Artifacts) != 1 || !strings.Contains(sum.Artifacts[0].Path, fmt.Sprintf("fsblk%d.bin", blk)) {
		t.Fatalf("artifacts %d: %+v", len(sum.Artifacts), sum.Artifacts)
	}
	before := len(auditEntries(t, s.Case))
	for name, o := range map[string]examine.JournalBlocksOptions{
		"unknown block": {JournalOptions: jo, Seq: seq, FSBlock: new(uint64)},
		"wrong seq":     {JournalOptions: jo, Seq: seq + 1},
	} {
		if _, err := s.JournalBlocks(context.Background(), o); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
	if got := len(auditEntries(t, s.Case)); got != before {
		t.Errorf("refused runs audited %d entries", got-before)
	}
	if _, err := s.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: seq, MaxFiles: -1}); !errors.Is(err, examine.ErrInvalidLimit) {
		t.Errorf("negative limit: %v", err)
	}
}

func TestJournalBlocksLimit(t *testing.T) {
	s, _, _ := jrnSession(t)
	sum := jrnBlocks(t, s, examine.JournalBlocksOptions{JournalOptions: jo, Seq: jrnSeq(t, s), MaxFiles: 1})
	if len(sum.Artifacts) != 1 || sum.OutputLimit != "max-files" {
		t.Errorf("%d artifacts, limit %q; want 1 and max-files", len(sum.Artifacts), sum.OutputLimit)
	}
	verifyOK(t, s.Case)
}

func TestJournalBlocksNotFreeIsSkipped(t *testing.T) {
	var live uint32
	s, _, _ := jrnSession(t, func(img []byte, tr *f2fstest.Tree) {
		live = tr.ChainAddrs[0]
		f2fstest.SetSITBit(img, f2Opts(rwOpts()), live, true)
	})
	chain, _ := jrnChain(t, s)
	sum := jrnBlocks(t, s, examine.JournalBlocksOptions{JournalOptions: jo, Seq: jrnSeq(t, s)})
	if sum.NotFree != 1 || sum.Copied != chain.Nodes-1 || len(sum.Artifacts) != chain.Nodes-1 || sum.Skipped != 1 {
		t.Fatalf("not-free %d copied %d artifacts %d skipped %d, want 1, %d, %d, 1", sum.NotFree, sum.Copied, len(sum.Artifacts), sum.Skipped, chain.Nodes-1, chain.Nodes-1)
	}
	for _, rec := range sum.Artifacts {
		if strings.Contains(rec.Path, fmt.Sprintf("fsblk%d.bin", live)) {
			t.Errorf("the allocated block %d was copied", live)
		}
	}
	warned := false
	for _, e := range auditByAction(t, s.Case, "analysis.warning") {
		warned = warned || strings.Contains(fmt.Sprint(e.Details["reason"]), "not-free")
	}
	if !warned {
		t.Error("no not-free analysis.warning")
	}
	verifyOK(t, s.Case)
}

func TestJournalNeverChangesLiveView(t *testing.T) {
	s, _, _ := jrnSession(t)
	view := func() (string, string) {
		fresh, err := examine.Open(s.Case, s.Parent.ID, examine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = fresh.Close() }()
		fsys, part, err := fresh.FS(-1)
		if err != nil {
			t.Fatal(err)
		}
		var tree strings.Builder
		err = filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, werr error) error {
			fmt.Fprintf(&tree, "%s %v %d %v\n", p, e.Type, e.Size, werr)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		runs, err := fsys.Unallocated()
		if err != nil {
			t.Fatal(err)
		}
		return tree.String(), fmt.Sprint(part.Start, runs)
	}
	t1, u1 := view()
	jrnReport(t, s, 0)
	jrnBlocks(t, s, examine.JournalBlocksOptions{JournalOptions: jo, Seq: jrnSeq(t, s)})
	t2, u2 := view()
	if t1 != t2 || u1 != u2 || t1 == "" {
		t.Errorf("the live view changed:\n%s\n%s\nunallocated %s -> %s", t1, t2, u1, u2)
	}
}
