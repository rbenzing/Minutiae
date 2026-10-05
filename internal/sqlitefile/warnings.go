package sqlitefile

import (
	"sync"
	"unicode/utf8"
)

// FileKind names which file of a database a location or warning refers to.
type FileKind uint8

// The files a database is read from.
const (
	FileDB      FileKind = 1
	FileWAL     FileKind = 2
	FileJournal FileKind = 3
)

func (k FileKind) String() string {
	switch k {
	case FileDB:
		return "db"
	case FileWAL:
		return "wal"
	case FileJournal:
		return "journal"
	}
	return "unknown"
}

// Warning codes: a closed set (the Format reference). They are exported so
// that callers can switch on them; TestWarningCodesPinned pins the set.
const (
	WarnTruncatedFile           = "truncated-file"
	WarnHdrFractions            = "hdr-fractions"
	WarnHdrEncodingInvalid      = "hdr-encoding-invalid"
	WarnHdrVersionBytes         = "hdr-version-bytes"
	WarnHdrCounterMismatch      = "hdr-counter-mismatch"
	WarnPageCountClamped        = "page-count-clamped"
	WarnPageUnavailable         = "page-unavailable"
	WarnPageTypeInvalid         = "page-type-invalid"
	WarnPageRange               = "page-range"
	WarnCellPointer             = "cell-pointer"
	WarnCellOverflowChain       = "cell-overflow-chain"
	WarnCellTooLarge            = "cell-too-large"
	WarnRecordInvalid           = "record-invalid"
	WarnRecordReservedSerial    = "record-reserved-serial"
	WarnBTreeCycle              = "btree-cycle"
	WarnBTreeDepth              = "btree-depth"
	WarnBTreeOrder              = "btree-order"
	WarnBTreeShape              = "btree-shape"
	WarnFreelistCycle           = "freelist-cycle"
	WarnFreelistCount           = "freelist-count"
	WarnFreelistLeafCount       = "freelist-leaf-count"
	WarnFreeblockChain          = "freeblock-chain"
	WarnPtrmapMismatch          = "ptrmap-mismatch"
	WarnSchemaRowInvalid        = "schema-row-invalid"
	WarnSchemaDuplicate         = "schema-duplicate"
	WarnSchemaSQLUnparsed       = "schema-sql-unparsed"
	WarnWALHeaderInvalid        = "wal-header-invalid"
	WarnWALPageSizeMismatch     = "wal-page-size-mismatch"
	WarnWALTornTail             = "wal-torn-tail"
	WarnWALModeMismatch         = "wal-mode-mismatch"
	WarnJournalHeaderInvalid    = "journal-header-invalid"
	WarnJournalSectorInvalid    = "journal-sector-invalid"
	WarnJournalPageSizeMismatch = "journal-page-size-mismatch"
	WarnJournalNoPageSize       = "journal-no-page-size"
	WarnJournalPageInvalid      = "journal-page-invalid"
	WarnJournalHot              = "journal-hot"
	WarnJournalSuperUnknown     = "journal-super-unknown"
	WarnJournalAndWAL           = "journal-and-wal"
	WarnJournalDuplicatePage    = "journal-duplicate-page"
	WarnSnapshotUnavailable     = "snapshot-unavailable"
	WarnOwnerChanged            = "owner-changed"
	WarnLimitReached            = "limit-reached"
	WarnSuppressed              = "suppressed"
)

// knownWarningCodes is the set the collector accepts (kept equal to the
// constants above by TestWarningCodesPinned).
var knownWarningCodes = map[string]bool{
	WarnTruncatedFile: true, WarnHdrFractions: true, WarnHdrEncodingInvalid: true, WarnHdrVersionBytes: true,
	WarnHdrCounterMismatch: true, WarnPageCountClamped: true, WarnPageUnavailable: true, WarnPageTypeInvalid: true,
	WarnPageRange: true, WarnCellPointer: true, WarnCellOverflowChain: true, WarnCellTooLarge: true,
	WarnRecordInvalid: true, WarnRecordReservedSerial: true, WarnBTreeCycle: true, WarnBTreeDepth: true, WarnBTreeOrder: true, WarnBTreeShape: true,
	WarnFreelistCycle: true, WarnFreelistCount: true, WarnFreelistLeafCount: true, WarnFreeblockChain: true,
	WarnPtrmapMismatch: true, WarnSchemaRowInvalid: true, WarnSchemaDuplicate: true, WarnSchemaSQLUnparsed: true,
	WarnWALHeaderInvalid: true, WarnWALPageSizeMismatch: true, WarnWALTornTail: true, WarnWALModeMismatch: true,
	WarnJournalHeaderInvalid: true, WarnJournalSectorInvalid: true, WarnJournalPageSizeMismatch: true,
	WarnJournalNoPageSize: true, WarnJournalPageInvalid: true, WarnJournalHot: true, WarnJournalSuperUnknown: true,
	WarnJournalAndWAL: true, WarnJournalDuplicatePage: true, WarnSnapshotUnavailable: true, WarnOwnerChanged: true,
	WarnLimitReached: true, WarnSuppressed: true,
}

// Warning is one anomaly found while reading. Msg is short; any text read
// from the disk in it is quoted. File is 0 (printed as unknown) for a warning
// that belongs to no file: a limit that was clamped, a page count that
// overflowed, the suppression note.
type Warning struct {
	Code   string
	File   FileKind
	Page   uint32
	Offset int64
	Msg    string
}

type warningKey struct {
	code   string
	file   FileKind
	page   uint32
	offset int64
	msg    string
}

// maxWarningMsg is the longest message a warning keeps; longer ones are cut
// at a rune boundary and end in clippedMark, so a producer that quotes a
// long on-disk name cannot make 1000 warnings large.
const (
	maxWarningMsg = 256
	clippedMark   = " [clipped]"
)

// clipMsg returns m cut to at most maxWarningMsg bytes, marked when cut.
func clipMsg(m string) string {
	if len(m) <= maxWarningMsg {
		return m
	}
	cut := maxWarningMsg - len(clippedMark)
	for cut > 0 && !utf8.RuneStart(m[cut]) {
		cut--
	}
	return m[:cut] + clippedMark
}

// warnings is the mutex-safe collector: identical warnings (same code, file,
// page and message) collapse, and after the cap of distinct warnings exactly
// one WarnSuppressed line is added and the rest are dropped.
type warnings struct {
	mu         sync.Mutex
	limit      int
	seen       map[warningKey]struct{}
	list       []Warning
	suppressed bool
	// calls counts every add, duplicates and suppressed ones included: a damage
	// tally for callers that must know whether anything was met while they ran.
	calls int64
	// unknown, when set, is called for a code outside the closed set. Only
	// tests set it (to panic); production records the warning as given
	// rather than lose it.
	unknown func(code string)
}

// newWarnings returns a collector holding at most limit distinct warnings
// (limit <= 0: the default of 1000).
func newWarnings(limit int) *warnings {
	if limit <= 0 {
		limit = DefaultLimits().MaxWarnings
	}
	return &warnings{limit: limit, seen: map[warningKey]struct{}{}}
}

func (w *warnings) add(x Warning) {
	if !knownWarningCodes[x.Code] && w.unknown != nil {
		w.unknown(x.Code)
	}
	// Two long messages that differ only past the clip collapse into one: the
	// key is the clipped text.
	x.Msg = clipMsg(x.Msg)
	k := warningKey{x.Code, x.File, x.Page, x.Offset, x.Msg}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if _, dup := w.seen[k]; dup {
		return
	}
	if len(w.seen) >= w.limit {
		if !w.suppressed {
			w.suppressed = true
			w.list = append(w.list, Warning{Code: WarnSuppressed, Msg: "further warnings suppressed"})
		}
		return
	}
	w.seen[k] = struct{}{}
	w.list = append(w.list, x)
}

// callCount returns the number of add calls so far.
func (w *warnings) callCount() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// snapshot returns a copy of the warnings recorded so far, in the order they
// were first seen.
func (w *warnings) snapshot() []Warning {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Warning(nil), w.list...)
}
