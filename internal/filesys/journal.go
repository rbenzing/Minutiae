package filesys

import "fmt"

// Journaler is implemented by a filesystem that keeps a journal. Callers find it with As. Interface
// only in 3A: the readers implement it in later plans.
type Journaler interface {
	Journal() (JournalInfo, error)
	JournalTransactions(visit func(JournalTxn) bool) error
	JournalBlock(t JournalTxn, i int) ([]byte, Run, error)
}

// JournalInfo describes a filesystem journal as found.
type JournalInfo struct {
	Type          string // "jbd2"
	Version       int
	BlockSize     int
	Blocks        int64  // journal length in blocks
	Sequence      uint32 // first expected transaction
	Start         uint32 // 0 = clean (empty live region)
	Features      []string
	NeedsRecovery bool
	Warnings      []string

	// 3G fields. Zero means "not applicable".
	Incomplete bool   // the scan could not see everything
	Stop       string // why, as "<reason>[@<block>]"; "" when complete
}

// JournalTag is one block a transaction describes.
type JournalTag struct {
	FSBlock uint64
	Flags   uint32
	Revoked bool

	// 3G fields. Zero means "not applicable".
	JournalBlock uint64 // journal-relative block holding the logged copy
	Copy         string // verified | unverified | mismatch | missing | outside
	Escaped      bool   // the logged copy had its magic zeroed (JBD2 escape)
}

// JournalTxn is one transaction found in the journal. Region "live" is the log region a recovery scans,
// never live data; "stale" is physically present outside it.
type JournalTxn struct {
	Seq       uint32
	Committed bool
	Region    string // "live" or "stale"
	Blocks    []JournalTag
	Revoked   []uint64
	Warnings  []string

	// 3G fields. Zero means "not applicable" (the F2FS readers leave them zero).
	StartBlock  int64     // journal-relative block of the first block of the transaction; -1 none
	CommitBlock int64     // journal-relative block of the commit; -1 none
	CommitTime  Timestamp // from the commit block; zero when unknown
	Checksum    string    // ok | bad | not-recorded | not-verified
}

// ErrNoJournal means the filesystem keeps no journal.
var ErrNoJournal = fmt.Errorf("%w: filesystem keeps no journal", ErrUnsupported)

// AllocatedRunner is implemented by a File that knows its whole allocation (filesystem-relative, whole
// allocation units, file order, holes omitted), which may exceed what the size needs. Interface only: no
// consumer in 3A.
type AllocatedRunner interface {
	File
	AllocatedRuns() []Run
}

// AllocatedRunsOf returns the allocation of f when f (or the file a wrapper wraps) implements
// AllocatedRunner. A wrapper that cannot say reports it with HasAllocatedRuns() == false.
func AllocatedRunsOf(f File) ([]Run, bool) {
	if f == nil {
		return nil, false
	}
	if h, ok := f.(interface{ HasAllocatedRuns() bool }); ok && !h.HasAllocatedRuns() {
		return nil, false
	}
	a, ok := f.(AllocatedRunner)
	if !ok {
		return nil, false
	}
	return a.AllocatedRuns(), true
}
