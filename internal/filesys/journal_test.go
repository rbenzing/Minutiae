package filesys_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

type fakeJournal struct{}

func (fakeJournal) Journal() (filesys.JournalInfo, error)                   { return filesys.JournalInfo{}, nil }
func (fakeJournal) JournalTransactions(func(filesys.JournalTxn) bool) error { return nil }
func (fakeJournal) JournalBlock(filesys.JournalTxn, int) ([]byte, filesys.Run, error) {
	return nil, filesys.Run{}, nil
}

type allocFile struct {
	filesys.File
	runs []filesys.Run
}

func (a allocFile) AllocatedRuns() []filesys.Run { return a.runs }

// wrapFile forwards to an inner file and says whether it can report allocated runs.
type wrapFile struct {
	allocFile
	has bool
}

func (w wrapFile) HasAllocatedRuns() bool { return w.has }

type plainFile struct{ filesys.File }

var (
	_ filesys.Journaler       = fakeJournal{}
	_ filesys.AllocatedRunner = allocFile{}
)

func TestJournalTypesZeroValuesAreInert(t *testing.T) {
	var info filesys.JournalInfo
	if info.Type != "" || info.Start != 0 || info.NeedsRecovery || info.Blocks != 0 || info.Features != nil || info.Warnings != nil {
		t.Errorf("zero JournalInfo = %+v", info)
	}
	var txn filesys.JournalTxn
	if txn.Committed || txn.Blocks != nil || txn.Revoked != nil || txn.Region != "" || txn.Warnings != nil {
		t.Errorf("zero JournalTxn = %+v", txn)
	}
	var tag filesys.JournalTag
	if tag.Revoked || tag.FSBlock != 0 || tag.Flags != 0 {
		t.Errorf("zero JournalTag = %+v", tag)
	}
	var j filesys.Journaler = fakeJournal{}
	if _, err := j.Journal(); err != nil {
		t.Error(err)
	}
}

func TestAllocatedRunsOf(t *testing.T) {
	runs := []filesys.Run{{Offset: 4096, Length: 4096}}
	if got, ok := filesys.AllocatedRunsOf(allocFile{runs: runs}); !ok || len(got) != 1 || got[0] != runs[0] {
		t.Errorf("implementer: %v, %v", got, ok)
	}
	if got, ok := filesys.AllocatedRunsOf(plainFile{}); ok || got != nil {
		t.Errorf("non-implementer: %v, %v", got, ok)
	}
	if got, ok := filesys.AllocatedRunsOf(wrapFile{allocFile{runs: runs}, false}); ok || got != nil {
		t.Errorf("wrapper reporting false: %v, %v", got, ok)
	}
	if got, ok := filesys.AllocatedRunsOf(wrapFile{allocFile{runs: runs}, true}); !ok || len(got) != 1 {
		t.Errorf("wrapper reporting true: %v, %v", got, ok)
	}
	if got, ok := filesys.AllocatedRunsOf(nil); ok || got != nil {
		t.Errorf("nil file: %v, %v", got, ok)
	}
}
