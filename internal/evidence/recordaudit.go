package evidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

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
)

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
func (x IngestStart) Details() map[string]any { return detailsOf(x) }

// Details returns the audit details of the entry.
func (x BatchCommit) Details() map[string]any { return detailsOf(x) }

// Details returns the audit details of the entry.
func (x BatchFailure) Details() map[string]any { return detailsOf(x) }

// Details returns the audit details of the entry.
func (x IngestConclusion) Details() map[string]any { return detailsOf(x) }

// Details returns the audit details of the entry.
func (x IngestRecover) Details() map[string]any { return detailsOf(x) }

// DecodeDetails decodes the details of an audit entry (as read from the log, or
// as built by Details) into a codec type. Numbers must be integers where the
// type says so, and a field the type does not know is an error, so the codecs
// stay the single definition of what the log may hold. A field that is absent
// decodes to its zero value: verify compares every decoded value with the
// database, where a missing one cannot match.
func DecodeDetails[T any](d map[string]any) (T, error) {
	var out T
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
