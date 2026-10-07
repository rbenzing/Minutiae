package records

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

var (
	// ErrAlreadyIngested: a complete run of the same parser name and version
	// already covers a declared artifact (StartOptions.AllowReingest overrides).
	ErrAlreadyIngested = errors.New("artifact already ingested by this parser version")
	// ErrParserIdentityConflict: the same parser name and version was recorded
	// with a different hash.
	ErrParserIdentityConflict = errors.New("parser name and version already recorded with a different hash")
	// ErrWriterClosed: the ingest was ended or aborted.
	ErrWriterClosed = errors.New("records writer is closed")
	// ErrWriterNotStarted: the writer has no ingest yet (call Start).
	ErrWriterNotStarted = errors.New("records writer is not started")
	// ErrWriterStarted: Start was already called.
	ErrWriterStarted = errors.New("records writer is already started")
	// ErrIngestActive: this Case already has a live ingest (a Writer that was
	// started and neither ended nor aborted); a second Start is refused.
	ErrIngestActive = errors.New("an ingest is already active in this case")
	// ErrInvalidOptions: a writer or start option is out of range.
	ErrInvalidOptions = errors.New("invalid writer options")
)

// Writer option limits.
const (
	defaultBatchRows  = 5000
	maxBatchRows      = 20000
	defaultBatchBytes = 32 << 20
	maxBatchBytes     = 1 << 30
	maxAnalysisID     = 128
	maxErrorText      = 2048
	maxWarnPath       = 4096
	maxWarnReason     = 1024

	// MaxWarningsPerIngest is the most analysis.warning entries (warnings and rejections together) one
	// ingest writes; parse.MaxWarnings must equal it.
	MaxWarningsPerIngest = 10000
	maxWarnings          = MaxWarningsPerIngest
)

// WriterOptions tune batching. A batch is flushed when it holds BatchRows records
// or about BatchBytes bytes.
type WriterOptions struct {
	BatchRows  int // default 5000, max 20000
	BatchBytes int // default 32 MiB
}

// IngestResult is what End and Abort report.
type IngestResult struct {
	IngestID string
	Outcome  string // complete | incomplete
	Batches  int
	Records  int64
	FirstID  int64 // 0 when no record was stored
	LastID   int64
	Rollup   string
	Types    map[string]int64
	Warnings int // analysis.warning entries appended through Warn
	// WarningsSuppressed counts the warnings dropped after the per-ingest cap.
	WarningsSuppressed int
	// Rejected counts the records refused: Add calls that failed validation (ErrInvalidRecord) and Reject calls.
	Rejected int
}

const (
	stateNew = iota
	stateStarted
	stateClosed
)

// Writer writes one ingest of records for one parser into the case. It follows
// the audit-before-write rule: every batch, the start, the end or error of the
// ingest and every recovery is appended (and fsynced) to the audit log before
// any row it describes exists in artifacts.db. Add is safe for concurrent use.
// One Writer per case at a time: Start treats every ingest the audit log
// announced and the database never concluded as dead and recovers it, so a
// second writer started while another is live would recover the live one.
type Writer struct {
	c   *evidence.Case
	p   Parser
	opt WriterOptions

	// hook is a test seam, called at "after-start-audit", "after-batch-audit", "after-suppression-began", "after-suppression-note",
	// "before-insert", "after-insert", "after-end-audit" and "before-abort-audit",
	// always outside any transaction.
	hook func(point string) error

	ingest atomic.Pointer[string] // the ingest id; readable from a hook while mu is held

	mu         sync.Mutex
	state      int
	poison     error
	analysisID string
	declared   map[string]artifactInfo
	known      map[string]bool // every artifact id in the manifest at Start
	parserHash *string
	next       int64 // first id of the next batch
	dbNext     int64 // records_meta.next_id and max(id)+1 as the database must hold them now (checked before every batch)
	batchNo    int   // number of the last batch audited (committed or failed)
	buf        []prepared
	bufBytes   int
	digests    []string // digests of the committed batches, in batch order
	nrecords   int64
	firstID    int64
	lastID     int64
	types      map[string]int64
	warnings   int
	warnCap    int  // 0 means maxWarnings
	warnSupp   int  // entries the cap kept out of the log (warnings and rejections)
	rejSupp    int  // of those, the rejections
	rejected   int  // records refused: Add failures wrapping ErrInvalidRecord and Reject calls
	began      bool // the "suppression began" note was written (the cap was hit)
	noted      bool // the end-of-ingest suppression note was written: nothing more can be counted
}

// NewWriter checks the case is at schema v2 (evidence.ErrNeedsUpgrade
// otherwise), the parser identity and the options.
func NewWriter(c *evidence.Case, p Parser, o WriterOptions) (*Writer, error) {
	if c == nil {
		return nil, errors.New("records: no case")
	}
	if err := c.RequireSchema(2); err != nil {
		return nil, err
	}
	if err := validateParser(p); err != nil {
		return nil, err
	}
	if o.BatchRows == 0 {
		o.BatchRows = defaultBatchRows
	}
	if o.BatchBytes == 0 {
		o.BatchBytes = defaultBatchBytes
	}
	if o.BatchRows < 1 || o.BatchRows > maxBatchRows {
		return nil, fmt.Errorf("%w: BatchRows %d must be 1..%d", ErrInvalidOptions, o.BatchRows, maxBatchRows)
	}
	if o.BatchBytes < 1 || o.BatchBytes > maxBatchBytes {
		return nil, fmt.Errorf("%w: BatchBytes %d must be 1..%d", ErrInvalidOptions, o.BatchBytes, maxBatchBytes)
	}
	w := &Writer{c: c, p: p, opt: o, types: map[string]int64{}}
	if p.Hash != "" {
		h := p.Hash
		w.parserHash = &h
	}
	return w, nil
}

// IngestID returns the id of the ingest ("" before Start). It never blocks.
func (w *Writer) IngestID() string {
	if s := w.ingest.Load(); s != nil {
		return *s
	}
	return ""
}

func (w *Writer) callHook(point string) error {
	if w.hook == nil {
		return nil
	}
	return w.hook(point)
}

// usable returns the error that stops an operation on a writer that is not in
// the started state or is poisoned.
func (w *Writer) usable() error {
	switch w.state {
	case stateNew:
		return ErrWriterNotStarted
	case stateClosed:
		return ErrWriterClosed
	}
	return w.poison
}

// Add validates r and buffers it; the batch is flushed (audited, then written)
// when it reaches the row or byte threshold. A record that fails validation is
// rejected with a typed error and nothing is buffered; the refusal is audited as
// a rejection first (an analysis.warning entry marked rejected=true, subject to
// the shared cap of Warn) and then counted (IngestResult.Rejected), also past the
// cap. When that audit entry cannot be written Add returns the audit error
// instead and nothing is counted. A poisoned or closed writer, an audit failure
// or a database failure is not a rejection. Once a batch failed, every later call
// returns that error and the caller must Abort.
func (w *Writer) Add(ctx context.Context, r Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.usable(); err != nil {
		return err
	}
	p, err := w.prepareRecord(r)
	if err != nil {
		if errors.Is(err, ErrInvalidRecord) {
			if rerr := w.rejectLocked(r.SourcePath, "record refused: "+err.Error()); rerr != nil {
				return rerr
			}
		}
		return err
	}
	p.row.ParserName, p.row.ParserVersion, p.row.ParserHash = w.p.Name, w.p.Version, w.parserHash
	w.buf = append(w.buf, p)
	w.bufBytes += p.approxBytes
	if len(w.buf) >= w.opt.BatchRows || w.bufBytes >= w.opt.BatchBytes {
		return w.flush(ctx)
	}
	return nil
}

// prepareRecord checks the artifact of r against the ingest's declaration and
// validates r. w.mu is held.
func (w *Writer) prepareRecord(r Record) (prepared, error) {
	art, ok := w.declared[r.ArtifactID]
	if !ok {
		if w.known[r.ArtifactID] {
			return prepared{}, fmt.Errorf("%w: %q", ErrUndeclaredArtifact, clip(r.ArtifactID))
		}
		if r.ArtifactID != "" {
			return prepared{}, fmt.Errorf("%w: %q", ErrUnknownArtifact, clip(r.ArtifactID))
		}
	}
	return prepare(r, art)
}

// Flush writes the buffered records as one batch now.
func (w *Writer) Flush(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.usable(); err != nil {
		return err
	}
	return w.flush(ctx)
}

// Warn appends an analysis.warning entry (the shape examine uses) for path. Both
// texts are bounded (path 4096 bytes, reason 1024, cut on a rune boundary and
// marked with "..."), NUL and invalid UTF-8 become "?". A parser is an untrusted
// caller and the audit log can never shrink, so at most 10,000 warnings are
// written per ingest (rejections share the budget): later ones are only counted
// (IngestResult.WarningsSuppressed) and the ingest's conclusion is preceded by a
// single "further warnings suppressed" note that states how many were dropped.
func (w *Writer) Warn(_ context.Context, path, reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.warnLocked(path, reason, false)
}

// Reject records that the caller refused a record before Add. It goes through
// the same path as Warn (same cleaning, same per-ingest cap shared with Warn) and
// is audited first: rejected is incremented only after the warning entry was
// appended or, past the cap, after the rejection was counted for the
// end-of-ingest suppression note. When the audit append fails the error is
// returned and nothing is counted. A rejection is counted even when its
// individual entry is suppressed by the cap. A record that Add refused for being
// invalid is already audited and counted by Add: the caller does not Reject it
// again. Like Warn it also works on a poisoned writer. Its audit entry is marked
// rejected=true, so verify can prove every rejection: the entries plus the
// suppressed rejections the note carries equal the rejected count.
func (w *Writer) Reject(_ context.Context, path, reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rejectLocked(path, reason)
}

// rejectLocked audits a rejection (or, past the cap, counts it for the note) and
// only then counts it in rejected. w.mu is held.
func (w *Writer) rejectLocked(path, reason string) error {
	if err := w.warnLocked(path, reason, true); err != nil {
		return err
	}
	w.rejected++
	return nil
}

// warnLocked is the body of Warn and Reject. Every entry names its ingest (a
// parser cannot set these keys): verify and recovery count the entries per
// ingest and a rejection entry carries rejected=true. Past the cap nothing is
// written: the entry is counted (warnSupp, and rejSupp for a rejection) and the
// one suppression note, written when the ingest concludes (writeNote), carries
// the numbers. Once that note exists nothing more can be counted, so a later
// call is refused. w.mu is held.
func (w *Writer) warnLocked(path, reason string, rejection bool) error {
	switch w.state {
	case stateNew:
		return ErrWriterNotStarted
	case stateClosed:
		return ErrWriterClosed
	}
	if w.noted {
		return fmt.Errorf("%w: the ingest is concluding", ErrWriterClosed)
	}
	limit := w.warnCap
	if limit <= 0 {
		limit = maxWarnings
	}
	if w.warnings >= limit {
		if err := w.writeBegan(limit); err != nil {
			return err
		}
		w.warnSupp++
		if rejection {
			w.rejSupp++
		}
		return nil
	}
	d := map[string]any{
		"analysis_id": w.analysisID, "path": cleanText(path, maxWarnPath), "reason": cleanText(reason, maxWarnReason),
		evidence.WarnKeyIngest: w.IngestID(),
	}
	if rejection {
		d[evidence.WarnKeyRejected] = true
	}
	_, err := w.c.Audit.Append(evidence.ActionAnalysisWarning, "", d)
	if err != nil {
		return err
	}
	w.warnings++
	return nil
}

// writeBegan appends the "suppression began" note the first time the shared cap keeps an entry out
// of the log, before that entry is counted. It carries no numbers; it exists so that an ingest that
// dies before writeNote still shows in the audit log that suppression began, and recovery then
// records the suppressed counts as unknown instead of 0. A failed append returns the error and
// counts nothing (the next call tries again). w.mu is held.
func (w *Writer) writeBegan(limit int) error {
	if w.began {
		return nil
	}
	if _, err := w.c.Audit.Append(evidence.ActionAnalysisWarning, "", map[string]any{
		"analysis_id": w.analysisID, "path": "", evidence.WarnKeyIngest: w.IngestID(), evidence.WarnKeySuppressionBegan: true,
		"reason": fmt.Sprintf("warning cap reached (%d per ingest): further warnings and rejections are counted, not written; the totals follow when the ingest concludes", limit),
	}); err != nil {
		return fmt.Errorf("audit suppression began note: %w", err)
	}
	w.began = true
	return w.callHook("after-suppression-began")
}

// writeNote appends the one suppression note of the ingest, when the cap kept any
// entry out of the log: it carries the number of suppressed warnings and of
// suppressed rejections, so verify can check warnings_suppressed and rejected for
// equality. It runs once, before the conclusion is audited; a failure leaves the
// ingest unconcluded. w.mu is held.
func (w *Writer) writeNote() error {
	if w.noted || w.warnSupp == 0 {
		return nil
	}
	limit := w.warnCap
	if limit <= 0 {
		limit = maxWarnings
	}
	if _, err := w.c.Audit.Append(evidence.ActionAnalysisWarning, "", map[string]any{
		"analysis_id": w.analysisID, "path": "", evidence.WarnKeyIngest: w.IngestID(), evidence.WarnKeySuppression: true,
		"reason": fmt.Sprintf("further warnings suppressed (the cap is %d per ingest): %d warnings and %d rejections not written",
			limit, w.warnSupp-w.rejSupp, w.rejSupp),
		evidence.WarnKeySuppressedWarnings: w.warnSupp - w.rejSupp, evidence.WarnKeySuppressedRejects: w.rejSupp,
	}); err != nil {
		return fmt.Errorf("audit suppression note: %w", err)
	}
	w.noted = true
	return w.callHook("after-suppression-note")
}

// cleanText replaces NUL and invalid UTF-8 with "?" and bounds s to n bytes.
func cleanText(s string, n int) string {
	return clipTo(strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", "?"), "?"), n)
}

// fail records the failure of an announced batch: the batch.error entry is
// audited (best effort: when the audit log itself is failing there is nothing
// more to write), the writer is poisoned and the unwritten records are dropped.
func (w *Writer) fail(batchNo int, cause error) error {
	_, aerr := w.c.Audit.Append(evidence.ActionBatchError, "", evidence.BatchFailure{
		IngestID: w.IngestID(), BatchNo: batchNo, Error: clipTo(cause.Error(), maxErrorText),
	}.Details())
	w.poison = cause
	if aerr != nil {
		w.poison = errors.Join(cause, fmt.Errorf("audit batch error: %w", aerr))
	}
	w.buf, w.bufBytes = nil, 0
	return w.poison
}

// flush writes the buffer as one batch: ids are assigned (and never reused), the
// batch is audited and fsynced, and only then are its rows written. w.mu is held.
func (w *Writer) flush(ctx context.Context) error {
	if len(w.buf) == 0 {
		return nil
	}
	n := len(w.buf)
	// before anything is audited: the database must still hold the counter this
	// writer wrote (an attacker may have changed it since Start), and the range
	// must not overflow. The batch entry is irrevocable, so a refusal comes first.
	if err := w.checkStoredNext(ctx); err != nil {
		w.poison = err
		w.buf, w.bufBytes = nil, 0
		return err
	}
	if w.next > maxRecordID-int64(n) {
		w.poison = fmt.Errorf("%w: the next record id would be %d, above the cap of %d", evidence.ErrIntegrity, w.next, int64(maxRecordID))
		w.buf, w.bufBytes = nil, 0
		return w.poison
	}
	first := w.next
	w.next += int64(n) // consumed even if the batch fails
	w.batchNo++
	batchNo := w.batchNo

	bd := evidence.NewBatchDigest()
	types := map[string]int64{}
	hashes := map[string]string{}
	var incomplete []string
	for i := range w.buf {
		row := &w.buf[i].row
		row.ID = first + int64(i)
		art := w.declared[row.ArtifactID]
		bd.Add(evidence.RowDigest(*row, art.SHA256))
		types[row.Type]++
		hashes[art.ID] = art.SHA256
		if art.Incomplete && !slices.Contains(incomplete, art.ID) {
			incomplete = append(incomplete, art.ID)
		}
	}
	slices.Sort(incomplete)
	if incomplete == nil {
		incomplete = []string{}
	}
	digest := bd.Sum()
	created := time.Now().UTC().Format(time.RFC3339Nano)

	if _, err := w.c.Audit.Append(evidence.ActionBatch, "", evidence.BatchCommit{
		IngestID: w.IngestID(), BatchNo: batchNo, FirstID: first, Count: n, Digest: digest, Created: created,
		Artifacts: hashes, ArtifactIncomplete: incomplete, Types: types,
	}.Details()); err != nil {
		return w.fail(batchNo, fmt.Errorf("audit batch: %w", err))
	}
	if err := w.callHook("after-batch-audit"); err != nil {
		return w.fail(batchNo, err)
	}
	if err := w.callHook("before-insert"); err != nil {
		return w.fail(batchNo, err)
	}
	if err := w.insertBatch(ctx, batchNo, first, digest, created); err != nil {
		return w.fail(batchNo, err)
	}

	w.dbNext = first + int64(n)
	w.digests = append(w.digests, digest)
	if w.nrecords == 0 {
		w.firstID = first
	}
	w.lastID = first + int64(n) - 1
	w.nrecords += int64(n)
	for t, c := range types {
		w.types[t] += c
	}
	w.buf, w.bufBytes = nil, 0
	if err := w.callHook("after-insert"); err != nil {
		w.poison = err
		return err
	}
	return nil
}

const insertRecordSQL = `INSERT INTO records (id, batch_id, type, payload_v, artifact_id, source_path, locator, src_offset, src_length,
	ts, ts_end, ts_basis, tz_offset_min, deleted, recovered, recovery_method, confidence, parser_id, summary, body, payload)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// insertBatch writes the batch row, its records, their times and the new
// next_id in one transaction.
func (w *Writer) insertBatch(ctx context.Context, batchNo int, first int64, digest, created string) error {
	n := len(w.buf)
	return w.c.StoreTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO record_batches (ingest_id, batch_no, first_id, count, digest, created) VALUES (?, ?, ?, ?, ?, ?)`,
			w.IngestID(), batchNo, first, n, digest, created)
		if err != nil {
			return err
		}
		batchID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		var parserID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM parsers WHERE name = ? AND version = ?`, w.p.Name, w.p.Version).Scan(&parserID); err != nil {
			return fmt.Errorf("parser row: %w", err)
		}
		ins, err := tx.PrepareContext(ctx, insertRecordSQL)
		if err != nil {
			return err
		}
		defer func() { _ = ins.Close() }()
		insTime, err := tx.PrepareContext(ctx, `INSERT INTO record_times (record_id, kind, ts, ts_basis, tz_offset_min) VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = insTime.Close() }()
		for i := range w.buf {
			r := &w.buf[i].row
			if _, err := ins.ExecContext(ctx, r.ID, batchID, r.Type, r.PayloadV, r.ArtifactID, r.SourcePath, r.Locator, r.SrcOffset, r.SrcLength,
				r.TS, r.TSEnd, r.TSBasis, r.TZOffsetMin, boolInt(r.Deleted), boolInt(r.Recovered), r.RecoveryMethod, r.Confidence,
				parserID, r.Summary, r.Body, r.Payload); err != nil {
				return fmt.Errorf("record %d: %w", r.ID, err)
			}
			for _, t := range r.Times {
				if _, err := insTime.ExecContext(ctx, r.ID, t.Kind, t.TS, t.Basis, t.TZOffsetMin); err != nil {
					return fmt.Errorf("record %d time %q: %w", r.ID, t.Kind, err)
				}
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE records_meta SET value = ? WHERE key = 'next_id'`, strconv.FormatInt(first+int64(n), 10))
		return err
	})
}

// conclusion builds the totals of what was committed so far.
func (w *Writer) conclusion(outcome string) evidence.IngestConclusion {
	types := make(map[string]int64, len(w.types))
	for k, v := range w.types {
		types[k] = v
	}
	return evidence.IngestConclusion{
		IngestID: w.IngestID(), Outcome: outcome, Batches: len(w.digests), Records: w.nrecords,
		FirstID: w.firstID, LastID: w.lastID, Rollup: evidence.IngestRollup(w.digests), Types: types,
		Warnings: w.warnings, WarningsSuppressed: w.warnSupp, Rejected: w.rejected,
	}
}

func (w *Writer) result(c evidence.IngestConclusion) IngestResult {
	return IngestResult{
		IngestID: c.IngestID, Outcome: c.Outcome, Batches: c.Batches, Records: c.Records, FirstID: c.FirstID,
		LastID: c.LastID, Rollup: c.Rollup, Types: c.Types, Warnings: c.Warnings, WarningsSuppressed: c.WarningsSuppressed,
		Rejected: c.Rejected,
	}
}

// End flushes the last batch, audits records.ingest.end and then records the run
// (outcome complete), its artifact coverage and the supersession it causes. A
// writer whose batch failed refuses End: Abort it instead.
func (w *Writer) End(ctx context.Context) (IngestResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.releaseIfClosed()
	if err := w.usable(); err != nil {
		return IngestResult{}, err
	}
	if err := w.flush(ctx); err != nil {
		return IngestResult{}, err
	}
	if err := w.writeNote(); err != nil {
		return IngestResult{}, err
	}
	concl := w.conclusion("complete")
	e, err := w.c.Audit.Append(evidence.ActionIngestEnd, "", concl.Details())
	if err != nil {
		return IngestResult{}, fmt.Errorf("audit ingest end: %w", err)
	}
	w.state = stateClosed
	if err := w.callHook("after-end-audit"); err != nil {
		return IngestResult{}, err
	}
	// the outcome is audited: record the run even if the caller's ctx is cancelled now
	if err := w.storeRun(context.WithoutCancel(ctx), concl, e); err != nil {
		return IngestResult{}, err
	}
	return w.result(concl), nil
}

// Abort concludes the ingest as incomplete: records.ingest.error is audited and
// the run is recorded (outcome incomplete; it never supersedes). Batches that
// were committed stay, flagged by that outcome; records still buffered are
// dropped. It records the run even when ctx is already cancelled. When Abort
// itself fails (the before-abort hook or the audit append) the writer is closed
// and the case's live-ingest slot is released; it is not retryable. The ingest
// stays unconcluded in the audit log and the next Start recovers it
// (records.ingest.recover).
func (w *Writer) Abort(ctx context.Context, cause error) (IngestResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.releaseIfClosed()
	switch w.state {
	case stateNew:
		return IngestResult{}, ErrWriterNotStarted
	case stateClosed:
		return IngestResult{}, ErrWriterClosed
	}
	ctx = context.WithoutCancel(ctx)
	concl := w.conclusion("incomplete")
	concl.Error = "aborted"
	if cause != nil {
		concl.Error = clipTo(cause.Error(), maxErrorText)
	}
	if err := w.callHook("before-abort-audit"); err != nil {
		w.abandon()
		return IngestResult{}, err
	}
	if err := w.writeNote(); err != nil {
		w.abandon()
		return IngestResult{}, err
	}
	e, err := w.c.Audit.Append(evidence.ActionIngestError, "", concl.Details())
	if err != nil {
		w.abandon()
		return IngestResult{}, fmt.Errorf("audit ingest error: %w", err)
	}
	w.buf, w.bufBytes = nil, 0
	w.state = stateClosed
	if err := w.storeRun(ctx, concl, e); err != nil {
		return IngestResult{}, err
	}
	return w.result(concl), nil
}

// abandon closes the writer after an Abort that could not audit its conclusion:
// buffered records are dropped and the caller's deferred releaseIfClosed frees the
// live-ingest slot. w.mu is held.
func (w *Writer) abandon() {
	w.buf, w.bufBytes = nil, 0
	w.state = stateClosed
}

// storeRun records the run row of a concluded ingest; the audit entry e is the
// concluding one (its seq is the run's end_seq).
func (w *Writer) storeRun(ctx context.Context, concl evidence.IngestConclusion, e evidence.AuditEntry) error {
	artifacts := make([]string, 0, len(w.declared))
	for id := range w.declared {
		artifacts = append(artifacts, id)
	}
	slices.Sort(artifacts)
	run := runRow{
		endSeq: e.Seq, ended: e.Time, parserName: w.p.Name, parserVersion: w.p.Version, parserHash: w.parserHash,
		analysisID: w.analysisID, conclusion: concl, artifacts: artifacts,
	}
	return w.c.StoreTx(ctx, func(tx *sql.Tx) error { return insertRun(ctx, tx, run) })
}

// runRow is everything a run row, its coverage and its supersession need.
type runRow struct {
	endSeq        int64
	ended         string
	parserName    string
	parserVersion string
	parserHash    *string
	analysisID    string
	conclusion    evidence.IngestConclusion
	artifacts     []string
}

// supersedeSQL records every supersession pair the runs now imply that
// record_superseded does not hold yet. The definition of a pair is
// evidence.SupersededPairsSQL, the one shared with verify and the reader.
const supersedeSQL = `INSERT INTO record_superseded (ingest_id, artifact_id)
	SELECT s.ingest_id, s.artifact_id FROM (` + evidence.SupersededPairsSQL + `) s
	WHERE NOT EXISTS (SELECT 1 FROM record_superseded x WHERE x.ingest_id = s.ingest_id AND x.artifact_id = s.artifact_id)`

// insertRun inserts the parser row (when absent), the run row, its coverage and
// the supersession pairs, inside tx.
func insertRun(ctx context.Context, tx *sql.Tx, r runRow) error {
	if err := ensureParser(ctx, tx, r.parserName, r.parserVersion, r.parserHash); err != nil {
		return err
	}
	c := r.conclusion
	var analysis any
	if r.analysisID != "" {
		analysis = r.analysisID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO record_runs (end_seq, ingest_id, parser_id, analysis_id, outcome, batches, records, first_id, last_id, rollup, ended)
		VALUES (?, ?, (SELECT id FROM parsers WHERE name = ? AND version = ?), ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.endSeq, c.IngestID, r.parserName, r.parserVersion, analysis, c.Outcome, c.Batches, c.Records, c.FirstID, c.LastID, c.Rollup, r.ended); err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	for _, id := range r.artifacts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO record_run_artifacts (ingest_id, artifact_id) VALUES (?, ?)`, c.IngestID, id); err != nil {
			return fmt.Errorf("insert run coverage %q: %w", clip(id), err)
		}
	}
	if _, err := tx.ExecContext(ctx, supersedeSQL); err != nil {
		return fmt.Errorf("record supersession: %w", err)
	}
	return nil
}

// ensureParser inserts the parsers row when absent and refuses a row with the
// same name and version but another hash.
func ensureParser(ctx context.Context, tx *sql.Tx, name, version string, hash *string) error {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO parsers (name, version, hash) VALUES (?, ?, ?)`, name, version, hash); err != nil {
		return fmt.Errorf("insert parser: %w", err)
	}
	var stored sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT hash FROM parsers WHERE name = ? AND version = ?`, name, version).Scan(&stored); err != nil {
		return fmt.Errorf("read parser: %w", err)
	}
	if !sameHash(stored, hash) {
		return fmt.Errorf("%w: %s %s", ErrParserIdentityConflict, clip(name), clip(version))
	}
	return nil
}

func sameHash(stored sql.NullString, hash *string) bool {
	if hash == nil {
		return !stored.Valid
	}
	return stored.Valid && stored.String == *hash
}

// clipTo shortens s to at most n bytes on a rune boundary.
func clipTo(s string, n int) string {
	if len(s) <= n {
		return strings.ToValidUTF8(s, "?")
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return strings.ToValidUTF8(s[:cut], "?") + "..."
}

// releaseIfClosed frees the Case's live-ingest slot once the ingest has
// concluded (End or Abort got as far as closing the writer). w.mu is held.
func (w *Writer) releaseIfClosed() {
	if w.state == stateClosed {
		w.c.EndIngest(w.IngestID())
	}
}

// checkStoredNext requires the database to hold exactly the counter this writer
// last wrote: records_meta.next_id == max(id)+1 == w.dbNext.
func (w *Writer) checkStoredNext(ctx context.Context) error {
	var got int64
	err := w.c.ReadRecordsTx(ctx, func(h evidence.ReadHandle) error {
		var err error
		got, err = storedNextID(ctx, h)
		return err
	})
	if err != nil {
		return err
	}
	if got != w.dbNext {
		return fmt.Errorf("%w: the highest record id is now %d, but this ingest last wrote up to %d (run: minutiae case verify --case %s)",
			evidence.ErrIntegrity, got-1, w.dbNext-1, w.c.Dir)
	}
	return nil
}
