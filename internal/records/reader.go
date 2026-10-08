package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
	// afterStart is a test seam, called after a statement has started and before its rows are read.
	afterStart func()
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
	// Provenance is the derivation chain of the artifact, checked against the manifest and the audit log.
	Provenance Provenance
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
	if needIndex {
		if err := ctxErr(ctx); err != nil { // R58: an expired deadline is a timeout before the first statement
			return mapTimeout(ctx, err)
		}
	}
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
		if err := fn(h); err != nil {
			return err
		}
		if needIndex { // R58: a deadline that passed while the calls ran is a timeout, never a late answer
			return ctxErr(ctx)
		}
		return nil
	})
	if needIndex {
		err = mapTimeout(ctx, err)
	}
	return r.integrity(err)
}

// integrity turns an error that says the database (or a full-text structure inside it) is
// corrupt into evidence.ErrIntegrity (exit 4) with the remedy named, whichever Reader call met it
// (R64); every other error is returned as it is.
func (r *Reader) integrity(err error) error {
	if err == nil || errors.Is(err, evidence.ErrIntegrity) {
		return err
	}
	if c := evidence.ClassifyDBError(err); errors.Is(c, evidence.ErrIntegrity) {
		return fmt.Errorf("%w (the case is damaged; run: minutiae case verify --case %s)", c, r.c.Dir)
	}
	return err
}

// ctxErr is ctx.Err(), and DeadlineExceeded also when the context's deadline has passed but its timer
// has not fired yet (a deadline of 1 ns is passed before any statement, whatever the timer says).
func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl) {
		return context.DeadlineExceeded
	}
	return nil
}

// matching is called right before each statement that carries the text query: it refuses an expired
// deadline (R58) and runs the test seam.
func (r *Reader) matching(ctx context.Context) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if r.beforeQuery != nil {
		r.beforeQuery()
	}
	return nil
}

// started runs the test seam that fires after a statement has started, before its rows are read.
func (r *Reader) started() {
	if r.afterStart != nil {
		r.afterStart()
	}
}

// mapTimeout turns an expired deadline into ErrSearchTimeout (wrapping the context error); a
// cancellation stays a cancellation.
// A statement the driver interrupts when the deadline passes may report its own interrupt error and
// not the context's: when the context's deadline has passed, any error that is not a cancellation is
// the timeout.
func mapTimeout(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, ErrSearchTimeout) || errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrSearchTimeout, err)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && isInterrupt(err) {
		return fmt.Errorf("%w: %w (%w)", ErrSearchTimeout, context.DeadlineExceeded, err)
	}
	return err
}

// isInterrupt reports whether err is the driver's report of an interrupted statement.
func isInterrupt(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "interrupt")
}
