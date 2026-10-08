package artparse

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Audit actions the host writes around a parse run. analysis.start, analysis.end and analysis.error are
// the shared analysis brackets (examine uses the same names); the records writer writes everything
// between parse.job.start and parse.job.end.
const (
	ActionAnalysisStart = "analysis.start"
	ActionAnalysisEnd   = "analysis.end"
	ActionAnalysisError = "analysis.error"
	ActionParseJobStart = "parse.job.start"
	ActionParseJobEnd   = "parse.job.end"
)

// detailsOf converts a codec struct to the map form Audit.Append takes. Numbers are json.Number, exactly
// as they come back from the log.
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

// AuditParser is the identity of a parser build in an audit entry.
type AuditParser struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Hash    string `json:"hash"`
}

// AuditSnapshot names the filesystem snapshot an artifact was extracted from.
type AuditSnapshot struct {
	Name string `json:"name"`
	Xid  uint64 `json:"xid"`
}

// AuditFilters are the selection a run was started with.
type AuditFilters struct {
	Parsers          []string `json:"parsers"`
	Artifacts        []string `json:"artifacts"`
	IncludeSnapshots bool     `json:"include_snapshots"`
}

// AuditLimits are the limits of the run (durations in milliseconds).
type AuditLimits struct {
	ProbeBytes     int64 `json:"probe_bytes"`
	MemInputMax    int64 `json:"mem_input_max"`
	MemInputTotal  int64 `json:"mem_input_total"`
	MemBudget      int64 `json:"mem_budget"`
	MaxRecords     int64 `json:"max_records"`
	MaxRejected    int   `json:"max_rejected"`
	MaxNotes       int   `json:"max_notes"`
	MaxLookupOpens int   `json:"max_lookup_opens"`
	ProbeTimeoutMS int64 `json:"probe_timeout_ms"`
	TimeoutMS      int64 `json:"timeout_ms"`
	GraceMS        int64 `json:"grace_ms"`
}

// AuditJobs counts the discovered jobs by status.
type AuditJobs struct {
	New           int `json:"new"`
	AlreadyParsed int `json:"already_parsed"`
	StaleBundle   int `json:"stale_bundle"`
	Unparsed      int `json:"unparsed"`
}

// AnalysisStart is the details of analysis.start of a parse run.
type AnalysisStart struct {
	AnalysisID      string        `json:"analysis_id"`
	Op              string        `json:"op"`
	Parsers         []AuditParser `json:"parsers"`
	Filters         AuditFilters  `json:"filters"`
	Reparse         bool          `json:"reparse"`
	MinutiaeVersion string        `json:"minutiae_version"`
	Commit          string        `json:"commit"`
	GoVersion       string        `json:"go_version"`
	Limits          AuditLimits   `json:"limits"`
	Jobs            AuditJobs     `json:"jobs"`
}

// Details returns the audit details of the entry.
func (x AnalysisStart) Details() map[string]any { return detailsOf(x) }

// BundleMember is one input of a job as audited at parse.job.start.
type BundleMember struct {
	ArtifactID  string         `json:"artifact_id"`
	SHA256      string         `json:"sha256"`
	LogicalPath string         `json:"logical_path"`
	Platform    string         `json:"platform"`
	Namer       string         `json:"namer"`
	Snapshot    *AuditSnapshot `json:"snapshot,omitempty"`
}

// JobStart is the details of parse.job.start, appended before any input of the job is read.
type JobStart struct {
	AnalysisID string                  `json:"analysis_id"`
	Job        int                     `json:"job"`
	Parser     AuditParser             `json:"parser"`
	Bundle     map[string]BundleMember `json:"bundle"`
	Explicit   bool                    `json:"explicit"`
}

// Details returns the audit details of the entry.
func (x JobStart) Details() map[string]any { return detailsOf(x) }

// AuditPanic is a recovered parser panic (value up to 1 KiB, stack up to 2 KiB).
type AuditPanic struct {
	Value string `json:"value"`
	Stack string `json:"stack"`
}

// JobEnd is the details of parse.job.end, appended after the ingest of the job concluded.
type JobEnd struct {
	AnalysisID         string            `json:"analysis_id"`
	Job                int               `json:"job"`
	Outcome            string            `json:"outcome"`
	Reason             string            `json:"reason"`
	IngestID           string            `json:"ingest_id"`
	Records            int64             `json:"records"`
	Rejected           int               `json:"rejected"`
	Warnings           int               `json:"warnings"`
	WarningsSuppressed int               `json:"warnings_suppressed"`
	DurationMS         int64             `json:"duration_ms"`
	Notes              map[string]string `json:"notes"`
	Error              string            `json:"error"`
	Panic              *AuditPanic       `json:"panic,omitempty"`
	Abandoned          bool              `json:"abandoned"`
	TimedOut           bool              `json:"timed_out"`
	Integrity          bool              `json:"integrity"`
	ArtifactIncomplete []string          `json:"artifact_incomplete"`
	Streamed           []string          `json:"streamed"` // inputs served from the file: only the re-hash after the job covers them
	Lookups            []LookupOpen      `json:"lookups"`
	Snapshot           *AuditSnapshot    `json:"snapshot,omitempty"`
}

// Details returns the audit details of the entry.
func (x JobEnd) Details() map[string]any { return detailsOf(x) }

// AnalysisEnd is the details of analysis.end of a parse run that ran to the end.
type AnalysisEnd struct {
	AnalysisID string         `json:"analysis_id"`
	Jobs       map[string]int `json:"jobs"` // jobs by outcome
	Records    int64          `json:"records"`
	Rejected   int            `json:"rejected"`
	Warnings   int            `json:"warnings"`
}

// Details returns the audit details of the entry.
func (x AnalysisEnd) Details() map[string]any { return detailsOf(x) }

// AnalysisError is the details of analysis.error of a parse run that stopped early.
type AnalysisError struct {
	AnalysisID string         `json:"analysis_id"`
	Error      string         `json:"error"`
	Jobs       map[string]int `json:"jobs"`
	Records    int64          `json:"records"`
	Rejected   int            `json:"rejected"`
	Warnings   int            `json:"warnings"`
	NotRun     int            `json:"not_run"`
}

// Details returns the audit details of the entry.
func (x AnalysisError) Details() map[string]any { return detailsOf(x) }

// HostWarning is the details of the analysis.warning the host itself writes for a job that is unparsed
// before an ingest exists.
type HostWarning struct {
	AnalysisID string `json:"analysis_id"`
	Path       string `json:"path"`
	Reason     string `json:"reason"`
	Source     string `json:"source"`
}

// Details returns the audit details of the entry.
func (x HostWarning) Details() map[string]any { return detailsOf(x) }
