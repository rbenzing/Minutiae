package sqlitefile

// TableBasis says how the table of a recovered row was established.
type TableBasis string

// The bases, strongest first.
const (
	BasisSchema TableBasis = "schema"
	BasisFit    TableBasis = "fit"
	BasisGuess  TableBasis = "guess"
	BasisNone   TableBasis = "none"
)

// Relation says how a recovered row relates to the live data.
type Relation string

// The relations.
const (
	RelAbsentFromLive    Relation = "absent-from-live"
	RelSupersededVersion Relation = "superseded-version"
	RelUncommitted       Relation = "uncommitted"
	RelUnknown           Relation = "unknown"
)

// The recovery method tokens. They are the values of records.Record.Recovery
// (internal/records/model.go), which this package must not import.
const (
	MethodWALPrior          = "sqlite-wal-prior"
	MethodJournalBefore     = "sqlite-journal-before"
	MethodWALStale          = "sqlite-wal-stale"
	MethodFreelist          = "sqlite-freelist"
	MethodWALUncommitted    = "sqlite-wal-uncommitted"
	MethodJournalRolledBack = "sqlite-journal-rolledback" // database pages a hot-journal rollback hides
	MethodJournalPersist    = "sqlite-journal-persist"
	MethodPageSlack         = "sqlite-page-slack" // produced by 3J; the rule lives here
)

// RecoveredRow is a row (or index entry) read from bytes the live state does
// not show. It is never live: it carries where its bytes lay (Loc, WAL,
// Journal), how its table was established (TableBasis) and, through Confidence,
// how far it can be trusted. A fit to a table schema is evidence, not proof.
type RecoveredRow struct {
	Method       string
	Table, Index string // "" when unknown; Index set for an index entry
	TableBasis   TableBasis
	Rowid        *int64 // nil when lost
	Values       []Value
	Loc          Loc
	WAL          *WALProv
	Journal      *JournalProv
	Relation     Relation
	OverflowHead uint32 // first overflow page when the chain was not followed
	Truncated    bool
	Notes        []string
}
