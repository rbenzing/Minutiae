package examine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// Journal listing, report, block copies and the R12 check (3H Task 10) through the real F2FS reader, on
// builder images whose warm-node log holds fsynced files that no checkpoint knows.

// jrnFiles is a live file and two post-checkpoint files.
func jrnFiles() []f2fstest.File {
	return []f2fstest.File{
		rwRF,
		{Path: "/live.bin", Data: f2Stamp(2*bs, "L")},
		{Path: "/rf/a.bin", Data: f2Stamp(3*bs, "a"), PostCheckpoint: true},
		{Path: "/rf/b.bin", Data: f2Stamp(2*bs, "b"), PostCheckpoint: true},
	}
}

func jrnSession(t *testing.T, patch ...func(img []byte, tr *f2fstest.Tree)) (*examine.Session, []byte, *f2fstest.Tree) {
	t.Helper()
	return f2Session(t, rwOpts(), jrnFiles(), patch...)
}

var jo = examine.JournalOptions{Partition: -1}

func TestJournalListOfChain(t *testing.T) {
	s, _, _ := jrnSession(t)
	beforeTree := hashTree(t, s.Case.Dir)
	beforeAudit := len(auditEntries(t, s.Case))
	l, err := s.JournalList(jo)
	if err != nil {
		t.Fatal(err)
	}
	if l.FSType != "f2fs" || l.Info.Type != "f2fs-rollforward" || len(l.Txns) != 1 || l.Chain == nil {
		t.Fatalf("listing %+v", l)
	}
	if len(l.Chain.Files) != 2 || l.Chain.Nodes == 0 || len(l.Txns[0].Blocks) != l.Chain.Nodes || !l.Chain.Complete {
		t.Errorf("chain %+v, txn blocks %d", l.Chain, len(l.Txns[0].Blocks))
	}
	if l.Txns[0].Seq != uint32(l.Chain.CheckpointVersion) || l.Txns[0].Region != "live" {
		t.Errorf("txn %+v", l.Txns[0])
	}
	if got := len(auditEntries(t, s.Case)); got != beforeAudit {
		t.Errorf("the audit log gained %d entries", got-beforeAudit)
	}
	after := hashTree(t, s.Case.Dir)
	if len(after) != len(beforeTree) {
		t.Errorf("case files %d -> %d", len(beforeTree), len(after))
	}
	for k, v := range beforeTree {
		if after[k] != v {
			t.Errorf("case file %s changed", k)
		}
	}
}

// 3G re-rules the journal rows for ext4: when the ext4 reader keeps a journal listing, drop the ext4 case here.
func TestJournalNotAvailableNeverSaysNoJournal(t *testing.T) {
	ext4Img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true, Label: "root"},
		[]ext4test.File{{Path: "/hello.txt", Data: []byte("hello")}})
	fatImg := fattest.Build(fattest.Options{Type: 16}, []fattest.File{{Path: "A.TXT", Data: []byte("hi")}})
	for _, tc := range []struct {
		name string
		img  []byte
		want string
	}{{"ext4", ext4Img, "ext4"}, {"fat", fatImg, "fat"}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := imageSession(t, tc.img)
			_, err := s.JournalList(jo)
			if !errors.Is(err, examine.ErrJournalNotAvailable) || !errors.Is(err, filesys.ErrUnsupported) {
				t.Fatalf("err = %v, want ErrJournalNotAvailable wrapping ErrUnsupported", err)
			}
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "keeps no journal") {
				t.Errorf("text %q: must name %q and never claim the filesystem keeps no journal", err, tc.want)
			}
			if _, err := s.JournalReport(context.Background(), jo, 0); !errors.Is(err, examine.ErrJournalNotAvailable) {
				t.Errorf("JournalReport err = %v", err)
			}
			if got := len(auditByAction(t, s.Case, "analysis.start")); got != 0 {
				t.Errorf("%d analysis.start entries for a refused run", got)
			}
		})
	}
}

func TestJournalShowWrongSeqIsNotFound(t *testing.T) {
	s, _, _ := jrnSession(t)
	l, err := s.JournalList(jo)
	if err != nil {
		t.Fatal(err)
	}
	seq := l.Txns[0].Seq
	n := 0
	chain, err := s.JournalShow(jo, seq, func(filesys.RollForwardNode) bool { n++; return true })
	if err != nil || n != chain.Nodes || n == 0 {
		t.Fatalf("show: %d nodes, chain %+v, err %v", n, chain, err)
	}
	called := false
	if _, err := s.JournalShow(jo, seq+1, func(filesys.RollForwardNode) bool { called = true; return true }); !errors.Is(err, filesys.ErrNotFound) || called {
		t.Errorf("wrong seq: err %v, visited %v", err, called)
	}
}

// An incomplete chain cannot say a sequence is absent: the nodes after its stop are unknown.
func TestJournalSeqOnIncompleteChainIsIncomplete(t *testing.T) {
	cut := func(img []byte, tr *f2fstest.Tree) {
		f2fstest.SetFooter(img, tr.ChainAddrs[len(tr.ChainAddrs)-1]+1, f2fstest.Footer{NID: 20, Ino: 0, CPVer: f2fstest.ChainFooterVersion(img)})
	}
	cutFirst := func(img []byte, tr *f2fstest.Tree) {
		f2fstest.SetFooter(img, tr.ChainAddrs[0], f2fstest.Footer{NID: 20, Ino: 0, CPVer: f2fstest.ChainFooterVersion(img)})
	}
	for name, patch := range map[string]func([]byte, *f2fstest.Tree){"past the end": cut, "empty chain": cutFirst} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := jrnSession(t, patch)
			chain, _ := jrnChain(t, s)
			if chain.Complete {
				t.Fatalf("chain %+v is complete", chain)
			}
			seq := uint32(chain.CheckpointVersion) + 1
			called := false
			_, err := s.JournalShow(jo, seq, func(filesys.RollForwardNode) bool { called = true; return true })
			if !errors.Is(err, filesys.ErrIncomplete) || errors.Is(err, filesys.ErrNotFound) || called || !strings.Contains(err.Error(), chain.Stop) {
				t.Errorf("JournalShow: err %v, visited %v; want ErrIncomplete naming %q", err, called, chain.Stop)
			}
			n := len(auditEntries(t, s.Case))
			_, err = s.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: seq})
			if !errors.Is(err, filesys.ErrIncomplete) || errors.Is(err, filesys.ErrNotFound) || !strings.Contains(err.Error(), chain.Stop) {
				t.Errorf("JournalBlocks: err %v; want ErrIncomplete naming %q", err, chain.Stop)
			}
			if len(auditEntries(t, s.Case)) != n {
				t.Errorf("a refused run was audited")
			}
		})
	}
}
