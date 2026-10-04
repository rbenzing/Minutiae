package evidence

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	ActionAnalysisWarning = "analysis.warning"       // analysis_id, path, reason (the shape examine uses)
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
	Error    string           `json:"error,omitempty"`
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

// Details returns the audit details of the entry.
func (x ReindexStart) Details() map[string]any { return detailsOf(x) }

// Details returns the audit details of the entry.
func (x ReindexDone) Details() map[string]any { return detailsOf(x) }

// Details returns the audit details of the entry.
func (x ReindexFailure) Details() map[string]any { return detailsOf(x) }
