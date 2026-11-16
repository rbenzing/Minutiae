package filesys

import "fmt"

// Journaler is implemented by a filesystem that keeps a journal. Callers find it with As. F2FS (3H) and ext4 (3G)
// implement it: F2FS over its roll-forward chain (see RollForwarder), ext4 over the jbd2 journal; other readers follow in later plans.
// The Run that JournalBlock returns is filesystem-relative (offset from the start of the filesystem,
// not of the image); the caller adds the partition start.
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
	Start         uint32 // 0 = clean (empty live region); F2FS reports the chain start block, even for an empty chain: the 0 = clean convention does not apply there
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
	Region    string // "live" or "stale"; "live" means the log region a recovery scans, never live data
	Blocks    []JournalTag
	Revoked   []uint64
	Warnings  []string

	// 3G fields. Zero means "not applicable" (the F2FS readers leave them zero).
	// StartBlock and CommitBlock are journal-relative; consumers must treat BOTH 0 and -1 as "none". Block 0 is the
	// journal superblock, so it is never a transaction block, and readers that leave the field unset (F2FS) give 0.
	StartBlock  int64     // block of the first block of the transaction
	CommitBlock int64     // block of the commit
	CommitTime  Timestamp // from the commit block; zero when unknown
	Checksum    string    // ok | bad | not-recorded | not-verified
}

// ErrNoJournal means the filesystem keeps no journal.
var ErrNoJournal = fmt.Errorf("%w: filesystem keeps no journal", ErrUnsupported)

// AllocatedRunner is implemented by a File that knows its whole allocation (filesystem-relative, whole
// allocation units, file order, holes omitted), which may exceed what the size needs. The contract is
// CheckAllocatedRuns; the slack export (3F) consumes it.
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
