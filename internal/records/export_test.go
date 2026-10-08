package records

import (
	"context"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Test exports of the unexported write-time validation.

type (
	ArtifactInfo = artifactInfo
	Prepared     = prepared
)

var (
	Prepare          = prepare
	CanonicalPayload = canonicalPayload
	CanonicalValue   = canonicalValue
	ValidateParser   = validateParser
)

// UnregisterType removes a type registered by a test.
func UnregisterType(name string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, name)
}

const (
	MaxSummary      = maxSummary
	MaxBody         = maxBody
	MaxPayload      = maxPayload
	MaxLocator      = maxLocator
	MaxSourcePath   = maxSourcePath
	MaxPayloadDepth = maxPayloadDepth
	MaxTimes        = maxTimes
)

// Row returns the prepared evidence row.
func (p prepared) Row() evidence.RecordRow { return p.row }

// ApproxBytes returns the writer's size estimate of the row.
func (p prepared) ApproxBytes() int { return p.approxBytes }

// SetHook installs a test seam called at the named points of a batch and of End
// ("after-start-audit", "after-batch-audit", "before-insert", "after-insert", "after-end-audit", "after-suppression-began", "after-suppression-note"),
// always outside any transaction. An error returned at the first two fails the
// batch; a panic simulates a process that died there.
func (w *Writer) SetHook(f func(point string) error) { w.hook = f }

// Die simulates the death of the process that ran the writer: the Case's
// live-ingest slot is released (a real dead process holds nothing), while the
// audit log and the database stay as they are.
func (w *Writer) Die() { w.c.EndIngest(w.IngestID()) }

// MaxWarnings is the default per-ingest cap on Warn entries.
const MaxWarnings = maxWarnings

// SetMaxWarnings lowers the Warn cap of this writer.
func (w *Writer) SetMaxWarnings(n int) { w.warnCap = n }

// Counts returns the writer's counters of written warnings, suppressed warnings
// and rejected records.
func (w *Writer) Counts() (warnings, suppressed, rejected int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.warnings, w.warnSupp, w.rejected
}

// Canonical exposes the cursor-fingerprint form of a compiled query.
func (q *TextQuery) Canonical() string { return q.canonical() }

// Fingerprint exposes the listing fingerprint of f for the case caseID.
func Fingerprint(f Filter, caseID string, desc bool) string { return f.fingerprint(caseID, desc) }

// SetBeforeQuery installs the seam called right before a full-text MATCH statement runs.
func (r *Reader) SetBeforeQuery(f func()) { r.beforeQuery = f }

// SnippetScanBytes is the most bytes of a text a snippet is built from.
const SnippetScanBytes = snippetScanBytes

// RankSQL returns the statement Search runs for a rank-order query.
func RankSQL(f Filter) string {
	q, err := buildRank(f, false)
	if err != nil {
		return "error: " + err.Error()
	}
	return q.sql
}

// SetAfterStart installs the seam called after a statement has started and before its rows are read.
func (r *Reader) SetAfterStart(f func()) { r.afterStart = f }

// MapTimeout exposes the deadline mapping of the Reader.
func MapTimeout(ctx context.Context, err error) error { return mapTimeout(ctx, err) }
