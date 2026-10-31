package f2fs

import "github.com/rbenzing/minutiae/internal/filesys"

// Journal, JournalTransactions and JournalBlock are stubs until plan 3H Task 5.
func (f *FS) Journal() (filesys.JournalInfo, error) { return filesys.JournalInfo{}, nil }

func (f *FS) JournalTransactions(_ func(filesys.JournalTxn) bool) error { return nil }

func (f *FS) JournalBlock(_ filesys.JournalTxn, _ int) ([]byte, filesys.Run, error) {
	return nil, filesys.Run{}, filesys.ErrNotFound
}
