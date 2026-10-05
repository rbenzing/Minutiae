package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/evidence"
)

var (
	// ErrBadCursor: the cursor is not one this reader produced for this case,
	// filter, order and direction (tampered, forged, truncated, another version).
	ErrBadCursor = errors.New("invalid cursor")
	// ErrInvalidPage: Page.Limit is negative or above MaxLimit.
	ErrInvalidPage = errors.New("invalid page")
	// ErrInvalidFilter: a filter value out of range (an IN list over 1,000
	// values, an empty parser name, an invalid time or text, an unknown stats key).
	ErrInvalidFilter = errors.New("invalid filter")
	// ErrNotFound: there is no record with that id.
	ErrNotFound = errors.New("record not found")
)

// Page sizes.
const (
	DefaultLimit = 100
	MaxLimit     = 10000
)

// Reader reads the records of a case. It never writes: every read is one
// evidence.Case.ReadTx (query_only, rolled back), appends nothing to the audit
// log and holds the case's single connection only for the duration of one call.
type Reader struct {
	c *evidence.Case
	// beforeQuery is a test seam, called right before a full-text MATCH statement runs.
	beforeQuery func()
}

// NewReader returns a reader for c. A case whose schema is older than v2 has no
// record tables: the error wraps evidence.ErrNeedsUpgrade.
func NewReader(c *evidence.Case) (*Reader, error) {
	if err := c.RequireSchema(2); err != nil {
		return nil, err
	}
	return &Reader{c: c}, nil
}

// Tri is a three-way filter: Any (the zero value), Only or None.
type Tri int

const (
	Any Tri = iota
	Only
	None
)

// ParserRef selects a parser by name and, when Version is not empty, version.
type ParserRef struct{ Name, Version string }

// Page selects one page of a listing.
type Page struct {
	Limit  int    // 0 = DefaultLimit; negative or above MaxLimit is ErrInvalidPage
	Cursor string // NextCursor of the previous page ("" = the first page)
	Desc   bool   // the exact reverse order; a cursor is only valid for its own filter, direction and case
}

// ParserInfo is the identity of the parser that produced a record.
type ParserInfo struct{ Name, Version, Hash string }

// Row is one record without its body and payload.
type Row struct {
	ID         int64
	Type       string
	PayloadV   int
	ArtifactID string
	SourcePath string // "" = the artifact itself
	Locator    string // "" = none
	Range      *Range // nil = no byte range
	TS, TSEnd  *Time  // nil = untimed / no end
	Deleted    bool
	Recovered  bool
	Method     string // recovery method; "" unless Recovered
	Confidence *int
	Parser     ParserInfo
	Summary    string
	IngestID   string
	Superseded bool // a later complete run of this parser covers this record's artifact
}

// Result is one page.
type Result struct {
	Rows       []Row
	NextCursor string // "" = no further page
}

// BatchInfo is the batch a record was written in.
type BatchInfo struct {
	IngestID string
	BatchNo  int
	Digest   string
	FirstID  int64
	Count    int
	Created  string
	AuditSeq int64 // audit sequence of the records.batch entry; 0 when the log holds none
}

// RunInfo is the ingest run a record belongs to (a record_runs row).
type RunInfo struct {
	ID            int64 // the run row's key: the audit sequence of its concluding entry
	IngestID      string
	AnalysisID    string
	Parser        string
	ParserVersion string
	ParserHash    string
	Outcome       string // complete | incomplete | interrupted
	Batches       int
	Records       int64
	FirstID       int64
	LastID        int64
	Ended         string
	AuditSeq      int64 // audit sequence of the concluding entry (equal to ID)
}

// Full is a record with everything stored for it and its provenance.
type Full struct {
	Row
	Body               string
	Payload            json.RawMessage // the canonical bytes exactly as stored
	Times              []evidence.RecordTime
	Artifact           evidence.ManifestRecord
	ArtifactIncomplete bool
	Batch              BatchInfo
	Run                *RunInfo // nil when the database holds no run row for the batch's ingest
	// SupersededBy is the ingest id of the lowest-ending later complete run of
	// this parser name that covers this record's artifact; "" when current.
	SupersededBy string
}

// StatRow is one group of Stats.
type StatRow struct {
	Key          string
	Count        int64
	TSMin, TSMax *int64 // Unix microseconds; nil when every row of the group is untimed
}

// Overview summarises the records a filter selects.
type Overview struct {
	Records int64
	// Deleted counts every deleted record, recovered ones included; Stats by
	// "deleted" puts recovered records in their own "recovered" group.
	Deleted   int64
	Recovered int64
	Untimed   int64
	// SupersededRecords counts the selected records that belong to a superseded
	// run, whether or not the filter hides them (so with the default filter it
	// is the number of records hidden by supersession).
	SupersededRecords int64
	TSMin, TSMax      *int64           // Unix microseconds
	Runs              map[string]int64 // every run of the case, by outcome
}

// readTx runs fn in one evidence.Case.ReadRecordsTx. With needIndex it first requires, inside the same
// snapshot, a schema of v3 or newer (ErrNeedsUpgrade otherwise) and a current full-text index
// (ErrIndexNotCurrent otherwise): every call that carries a text query goes through it.
func (r *Reader) readTx(ctx context.Context, needIndex bool, fn func(evidence.ReadHandle) error) error {
	err := r.c.ReadRecordsTx(ctx, func(h evidence.ReadHandle) error {
		if needIndex {
			var v int
			if err := h.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
				return fmt.Errorf("artifacts.db schema_version: %w", err)
			}
			if v < 3 {
				return fmt.Errorf("%w: case schema v%d < v3 has no full-text index; run: minutiae case upgrade --case %s", evidence.ErrNeedsUpgrade, v, r.c.Dir)
			}
			if err := evidence.RequireIndexCurrentIn(ctx, h); err != nil {
				return err
			}
		}
		return fn(h)
	})
	if needIndex {
		err = mapTimeout(err)
	}
	return err
}

// matching runs the test seam, called right before a full-text MATCH statement.
func (r *Reader) matching() {
	if r.beforeQuery != nil {
		r.beforeQuery()
	}
}

// mapTimeout turns an expired deadline into ErrSearchTimeout (wrapping the context error); a
// cancellation stays a cancellation.
func mapTimeout(err error) error {
	if err != nil && errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrSearchTimeout) {
		return fmt.Errorf("%w: %w", ErrSearchTimeout, err)
	}
	return err
}
