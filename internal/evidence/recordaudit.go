package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// RecordsAuditLayout is the layout version of the records.* audit details this build writes and reads.
const RecordsAuditLayout = 1

// recordsLayoutKey is the details key that carries the layout version.
const recordsLayoutKey = "layout"

// ErrAuditLayoutNewer is matched by an AuditLayoutError.
var ErrAuditLayoutNewer = errors.New("records audit entry written with a newer layout")

// AuditLayoutError is returned by DecodeDetails for details whose "layout" is above RecordsAuditLayout.
type AuditLayoutError struct{ Layout, Known int64 }

func (e *AuditLayoutError) Error() string {
	return fmt.Sprintf("records audit layout %d is newer than this build reads (up to %d)", e.Layout, e.Known)
}

// Unwrap makes the error match ErrAuditLayoutNewer.
func (e *AuditLayoutError) Unwrap() error { return ErrAuditLayoutNewer }

// withLayout adds the layout version to a details map built by detailsOf.
func withLayout(m map[string]any) map[string]any {
	m[recordsLayoutKey] = json.Number(strconv.Itoa(RecordsAuditLayout))
	return m
}

// auditProblemText is the verify text for details DecodeDetails refused: a newer layout is named as such
// (verify cannot check what this build cannot read, and says which build can), any other failure keeps
// the "details unreadable" text.
func auditProblemText(e AuditEntry, err error) string {
	var le *AuditLayoutError
	if errors.As(err, &le) {
		return fmt.Sprintf("audit seq %d: %s was written with records audit layout %d; this build reads layouts up to %d (verify it with the build that wrote it)",
			e.Seq, e.Action, le.Layout, le.Known)
	}
	return fmt.Sprintf("audit seq %d: %s details unreadable: %q", e.Seq, e.Action, err.Error())
}

// Audit actions of the unified artifact database. The case.upgrade actions are
// declared with the upgrade code (ActionCaseUpgrade and friends).
const (
	ActionIngestStart     = "records.ingest.start"   // IngestStart
	ActionBatch           = "records.batch"          // BatchCommit
	ActionBatchError      = "records.batch.error"    // BatchFailure
	ActionIngestEnd       = "records.ingest.end"     // IngestConclusion, outcome complete
	ActionIngestError     = "records.ingest.error"   // IngestConclusion, outcome incomplete, with error
	ActionIngestRecover   = "records.ingest.recover" // IngestRecover
	ActionAnalysisWarning = "analysis.warning"       // analysis_id, path, reason (the shape examine uses); the records writer adds ingest_id and, for a rejection and for the end-of-ingest suppression note, rejected / suppression (with the suppressed counts)
	ActionReindex         = "records.reindex"        // ReindexStart, before the index is touched
	ActionReindexDone     = "records.reindex.done"   // ReindexDone, after the index state was set to current
	ActionReindexError    = "records.reindex.error"  // ReindexFailure
)

// ReindexStart is the details of records.reindex: a rebuild of the full-text indexes announced (and
// fsynced) before any index row or the index state is changed.
type ReindexStart struct {
	ReindexID       string   `json:"reindex_id"`
	FromNormVersion string   `json:"from_norm_version"` // what records_meta held when the reindex began ("" when unbuilt or missing)
	NormVersion     string   `json:"norm_version"`      // what the index will be built with (FTSNormVersion)
	Records         int64    `json:"records"`           // rows of the records table
	Tables          []string `json:"tables"`            // the full-text tables that are dropped and rebuilt
}

// ReindexDone is the details of records.reindex.done: the rebuilt index is complete and its state is
// current. Docs is the number of documents each full-text table holds (its _docsize rows).
type ReindexDone struct {
	ReindexID      string           `json:"reindex_id"`
	NormVersion    string           `json:"norm_version"`
	RecordsIndexed int64            `json:"records_indexed"`
	Docs           map[string]int64 `json:"docs"`
}

// ReindexFailure is the details of records.reindex.error: a reindex that stopped. When any index row
// was dropped the index state stays "building" (nothing searches or writes it) until a reindex ends.
type ReindexFailure struct {
	ReindexID      string `json:"reindex_id"`
	Error          string `json:"error"`
	RecordsIndexed int64  `json:"records_indexed"`
}

// IngestStart is the details of records.ingest.start: an ingest announces the
// parser (name, version, hash) and the artifacts it will cover before any of its
// rows exist.
type IngestStart struct {
	IngestID      string   `json:"ingest_id"`
	Parser        string   `json:"parser"`
	ParserVersion string   `json:"parser_version"`
	ParserHash    string   `json:"parser_hash"` // "" means no hash (NULL in parsers); a stored hash is never ""
	AnalysisID    string   `json:"analysis_id"`
	Artifacts     []string `json:"artifacts"` // sorted, de-duplicated, required
	BatchRows     int      `json:"batch_rows"`
	Reingest      bool     `json:"reingest"`
	NormVersion   string   `json:"norm_version"` // the full-text index version the ingest indexes with (FTSNormVersion)
	// Sync is "normal" when the ingest ran its batches with synchronous=NORMAL (WriterOptions.RelaxedSync, ruling D89); absent means FULL.
	Sync string `json:"sync,omitempty"`
}

// BatchCommit is the details of records.batch: a batch announced (and fsynced)
// before its rows are written.
type BatchCommit struct {
	IngestID           string            `json:"ingest_id"`
	BatchNo            int               `json:"batch_no"`
	FirstID            int64             `json:"first_id"`
	Count              int               `json:"count"`
	Digest             string            `json:"digest"`
	Created            string            `json:"created"`   // the time the batch row is stored with (RFC 3339), committed before the row exists
	Artifacts          map[string]string `json:"artifacts"` // artifact id -> sha256
	ArtifactIncomplete []string          `json:"artifact_incomplete"`
	Types              map[string]int64  `json:"types"` // record type -> rows
}

// BatchFailure is the details of records.batch.error: an announced batch whose
// rows were not written.
type BatchFailure struct {
	IngestID string `json:"ingest_id"`
	BatchNo  int    `json:"batch_no"`
	Error    string `json:"error"`
}

// IngestConclusion is the details of records.ingest.end (Outcome "complete") and
// records.ingest.error (Outcome "incomplete", with Error): the totals the run row
// must equal.
type IngestConclusion struct {
	IngestID string           `json:"ingest_id"`
	Outcome  string           `json:"outcome"`
	Batches  int              `json:"batches"`
	Records  int64            `json:"records"`
	FirstID  int64            `json:"first_id"`
	LastID   int64            `json:"last_id"`
	Rollup   string           `json:"rollup"`
	Types    map[string]int64 `json:"types"`
	// Warnings, WarningsSuppressed and Rejected are audit-only counts (the run row holds no copy): the
	// analysis.warning entries written for the ingest, the warnings dropped after the per-ingest cap, and the
	// records refused (a failed Add with ErrInvalidRecord, or Writer.Reject). Entries written before these
	// fields existed lack the keys and decode to zero.
	Warnings           int `json:"warnings"`
	WarningsSuppressed int `json:"warnings_suppressed"`
	Rejected           int `json:"rejected"`
	// SuppressionUnknown is set only by recovery of an ingest that never concluded after it began suppressing:
	// how many entries the cap kept out of the log is then unknown (WarningsSuppressed is 0 and Rejected holds the
	// rejection entries only: lower bounds, not the true numbers).
	SuppressionUnknown bool   `json:"suppression_unknown,omitempty"`
	Error              string `json:"error,omitempty"`
}

// IngestRecover is the details of records.ingest.recover: the conclusion of an
// ingest that never concluded (Outcome "interrupted") or whose run row is
// missing, written by a later ingest (ByIngestID). BatchNos lists the announced
// batches that never reached the database.
type IngestRecover struct {
	IngestConclusion
	ByIngestID string `json:"by_ingest_id"`
	Reason     string `json:"reason"`
	BatchNos   []int  `json:"batch_nos"`
	RunMissing bool   `json:"run_missing"`
}

// detailsOf converts a codec struct to the map form Audit.Append takes. Numbers
// are json.Number, exactly as they come back from the log.
func detailsOf(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil { // cannot happen: the codec types hold only strings, numbers, bools, slices and maps of them
		panic(fmt.Sprintf("audit details of %T: %v", v, err))
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		panic(fmt.Sprintf("audit details of %T: %v", v, err))
	}
	return m
}

// Details returns the audit details of the entry.
func (x IngestStart) Details() map[string]any { return withLayout(detailsOf(x)) }

// Details returns the audit details of the entry.
func (x BatchCommit) Details() map[string]any { return withLayout(detailsOf(x)) }

// Details returns the audit details of the entry.
func (x BatchFailure) Details() map[string]any { return withLayout(detailsOf(x)) }

// Details returns the audit details of the entry.
func (x IngestConclusion) Details() map[string]any { return withLayout(detailsOf(x)) }

// Details returns the audit details of the entry.
func (x IngestRecover) Details() map[string]any { return withLayout(detailsOf(x)) }

// stripLayout returns d without its "layout" key (the caller's map is not changed) after checking it: absent
// means layout 1, an integer 1..RecordsAuditLayout is accepted, a larger integer is an *AuditLayoutError, and
// anything else (0, a negative, a fraction, text, a bool, null, a number beyond int64) is a plain error.
func stripLayout(d map[string]any) (map[string]any, error) {
	v, has := d[recordsLayoutKey]
	if !has {
		return d, nil
	}
	var n int64
	switch x := v.(type) {
	case json.Number:
		var err error
		if n, err = x.Int64(); err != nil {
			return nil, fmt.Errorf("audit details: layout %q is not an integer", x.String())
		}
	case float64:
		if x != math.Trunc(x) || x < 1 || x > 1<<53 {
			return nil, fmt.Errorf("audit details: layout %v is not a valid layout number", x)
		}
		n = int64(x)
	case int:
		n = int64(x)
	case int64:
		n = x
	default:
		return nil, fmt.Errorf("audit details: layout of type %T is not an integer", v)
	}
	if n < 1 {
		return nil, fmt.Errorf("audit details: layout %d is not a valid layout number", n)
	}
	if n > RecordsAuditLayout {
		return nil, &AuditLayoutError{Layout: n, Known: RecordsAuditLayout}
	}
	out := make(map[string]any, len(d))
	for k, x := range d {
		if k != recordsLayoutKey {
			out[k] = x
		}
	}
	return out, nil
}

// DecodeDetails decodes the details of an audit entry (as read from the log, or
// as built by Details) into a codec type. Numbers must be integers where the
// type says so, and a field the type does not know is an error, so the codecs
// stay the single definition of what the log may hold. A field that is absent
// decodes to its zero value: verify compares every decoded value with the
// database, where a missing one cannot match.
func DecodeDetails[T any](d map[string]any) (T, error) {
	var out T
	d, err := stripLayout(d)
	if err != nil {
		return out, err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return out, fmt.Errorf("audit details: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, fmt.Errorf("audit details: %w", err)
	}
	return out, nil
}

// Detail keys the records writer adds to its own analysis.warning entries, so
// that verify and recovery can count them without trusting any text a parser
// supplied: every writer entry names its ingest, an entry written for a
// rejection (Writer.Reject, or an Add refused for an invalid record) carries
// rejected=true, and the one "further warnings suppressed" note of an ingest,
// written when the ingest concludes, carries suppression=true and the numbers of
// entries the cap kept out of the log.
const (
	WarnKeyIngest      = "ingest_id"
	WarnKeyRejected    = "rejected"
	WarnKeySuppression = "suppression"
	// WarnKeySuppressionBegan marks the note the writer appends the moment the shared cap is first hit, before
	// anything is suppressed: it carries no numbers, it only proves that suppression began, so a crash before the
	// conclusion note is still visible to recovery and verify.
	WarnKeySuppressionBegan = "suppression_began"
	// WarnKeySuppressedWarnings and WarnKeySuppressedRejects are the numbers the
	// suppression note carries: the Warn calls and the rejections (Reject calls and
	// refused Adds) the cap kept out of the log.
	WarnKeySuppressedWarnings = "suppressed_warnings"
	WarnKeySuppressedRejects  = "suppressed_rejections"
)

// IngestWarningTally is what the audit log proves about the warnings of one
// ingest: Warnings analysis.warning entries (the suppression notes not counted),
// Rejects of them written for a rejection, Notes conclusion notes and Began
// suppression began notes (an ingest has at most one of each). NoteWarnings and NoteRejects sum what the notes say
// the cap kept out of the log; a note whose counts are missing, negative or not
// whole numbers adds nothing and is counted in NoteBadCounts, one whose counts
// are both zero in NoteEmpty.
type IngestWarningTally struct {
	Warnings      int
	Began         int // suppression began notes (an ingest has at most one)
	Rejects       int
	Notes         int
	NoteWarnings  int
	NoteRejects   int
	NoteBadCounts int
	NoteEmpty     int
}

// Suppressed is the number of entries the log proves the cap dropped.
func (t IngestWarningTally) Suppressed() int { return t.NoteWarnings + t.NoteRejects }

// ProvenRejected is the number of rejections the log proves: the rejection
// entries plus the suppressed rejections.
func (t IngestWarningTally) ProvenRejected() int { return t.Rejects + t.NoteRejects }

// maxNoteCount bounds a number read from a suppression note, so sums cannot overflow.
const maxNoteCount = 1 << 40

// noteCount reads a non-negative whole number from a decoded audit value.
func noteCount(v any) (int, bool) {
	var n int64
	switch x := v.(type) {
	case json.Number:
		var err error
		if n, err = x.Int64(); err != nil {
			return 0, false
		}
	case float64:
		if x != math.Trunc(x) || x < 0 || x > maxNoteCount {
			return 0, false
		}
		n = int64(x)
	case int:
		n = int64(x)
	default:
		return 0, false
	}
	if n < 0 || n > maxNoteCount {
		return 0, false
	}
	return int(n), true
}

// add counts one analysis.warning entry of the records writer.
func (t *IngestWarningTally) add(d map[string]any) {
	if b, _ := d[WarnKeySuppressionBegan].(bool); b {
		t.Began++
		return
	}
	if b, _ := d[WarnKeySuppression].(bool); b {
		t.Notes++
		w, wok := noteCount(d[WarnKeySuppressedWarnings])
		r, rok := noteCount(d[WarnKeySuppressedRejects])
		switch {
		case !wok || !rok:
			t.NoteBadCounts++
		case w+r == 0:
			t.NoteEmpty++
		default:
			t.NoteWarnings += w
			t.NoteRejects += r
		}
		return
	}
	t.Warnings++
	if b, _ := d[WarnKeyRejected].(bool); b {
		t.Rejects++
	}
}

// writerWarningIngest returns the ingest an analysis.warning entry written by the
// records writer names ("" for any other analysis.warning entry).
func writerWarningIngest(d map[string]any) string {
	s, _ := d[WarnKeyIngest].(string)
	return s
}

// Details returns the audit details of the entry.
func (x ReindexStart) Details() map[string]any { return withLayout(detailsOf(x)) }

// Details returns the audit details of the entry.
func (x ReindexDone) Details() map[string]any { return withLayout(detailsOf(x)) }

// Details returns the audit details of the entry.
func (x ReindexFailure) Details() map[string]any { return withLayout(detailsOf(x)) }
