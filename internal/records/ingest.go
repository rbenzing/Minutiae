package records

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// StartOptions announce an ingest.
type StartOptions struct {
	// AnalysisID ties the ingest to an analysis (carried by analysis.warning
	// entries and the run row); "" means none.
	AnalysisID string
	// Artifacts are the artifacts this ingest will cover: required, non-empty,
	// each in the manifest. Records may only point at these. Supersession works
	// on this coverage.
	Artifacts []string
	// AllowReingest permits an ingest of a parser name+version that already has a
	// complete run over a declared artifact. The start entry then says reingest:true.
	AllowReingest bool
}

func newIngestID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("ingest id: %w", err)
	}
	return "ing-" + hex.EncodeToString(b[:]), nil
}

// Start begins the ingest, in this order: it checks the schema, that the full-text
// index is current (ErrIndexNotCurrent otherwise; nothing is audited or recovered
// for a refused index) and the options, reads the manifest, RECOVERS every ingest
// the audit log announced and the database never concluded (see below), refuses a
// parser name+version that a
// complete run already covers (ErrAlreadyIngested, unless AllowReingest),
// checks the parser identity (ErrParserIdentityConflict), audits
// records.ingest.start and records the parser.
//
// While this Case has a live ingest (another Writer that was started and not
// yet ended or aborted) Start refuses with ErrIngestActive before it audits or
// recovers anything: a live ingest is never recovered. The ingest is live from a
// successful Start until End or Abort concludes it.
//
// Recovery comes first and is never undone: its records.ingest.recover entries
// and run rows stay even when Start then fails (the caller sees the error, the
// dead ingests are resolved anyway). The refusals and the identity check happen
// before the start entry is audited, so a refused ingest leaves no start entry.
// Recovery writes, for each unresolved ingest, a records.ingest.recover entry
// first and the run row after it; it never edits or deletes an existing row.
func (w *Writer) Start(ctx context.Context, so StartOptions) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch w.state {
	case stateStarted:
		return ErrWriterStarted
	case stateClosed:
		return ErrWriterClosed
	}
	if err := w.c.RequireSchema(3); err != nil {
		return err
	}
	// nothing is audited, recovered or written against a database whose schema
	// objects (tables, indexes, triggers, views) are not the ones this build defines
	if err := w.c.RequireSchemaObjects(ctx); err != nil {
		return err
	}
	// the full-text index must be current before anything is audited or recovered: the writer maintains
	// it in every batch transaction and cannot do that for an index another build or a rebuild owns
	if err := w.c.RequireIndexCurrent(ctx); err != nil {
		return err
	}
	if len(so.AnalysisID) > maxAnalysisID {
		return fmt.Errorf("%w: analysis id is %d bytes, the cap is %d", ErrInvalidOptions, len(so.AnalysisID), maxAnalysisID)
	}
	if err := checkText("analysis id", so.AnalysisID); err != nil {
		return err
	}
	artifacts := slices.Clone(so.Artifacts)
	slices.Sort(artifacts)
	artifacts = slices.Compact(artifacts)
	if len(artifacts) == 0 {
		return fmt.Errorf("%w: an ingest must declare the artifacts it covers", ErrInvalidOptions)
	}
	man, err := w.c.Manifest()
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	known := make(map[string]bool, len(man))
	byID := make(map[string]artifactInfo, len(man))
	for _, m := range man {
		known[m.ID] = true
		byID[m.ID] = artifactInfo{ID: m.ID, SHA256: m.SHA256, Size: m.Size, Incomplete: m.Incomplete}
	}
	declared := make(map[string]artifactInfo, len(artifacts))
	for _, id := range artifacts {
		if id == "" {
			return fmt.Errorf("%w: an empty artifact id", ErrUnknownArtifact)
		}
		info, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: %q is not in the manifest", ErrUnknownArtifact, clip(id))
		}
		declared[id] = info
	}

	ingestID, err := newIngestID()
	if err != nil {
		return err
	}
	if active, ok := w.c.BeginIngest(ingestID); !ok {
		return fmt.Errorf("%w: %s", ErrIngestActive, active)
	}
	defer func() {
		if w.state != stateStarted {
			w.c.EndIngest(ingestID)
		}
	}()

	unresolved, err := w.c.UnresolvedIngests()
	if err != nil {
		return err
	}
	for _, u := range unresolved {
		if err := w.recoverIngest(ctx, ingestID, u); err != nil {
			return fmt.Errorf("recover ingest %s: %w", u.Start.IngestID, err)
		}
	}

	reingest, next, err := w.preflight(ctx, artifacts, so.AllowReingest)
	if err != nil {
		return err
	}
	audited, err := auditedHighWater(w.c)
	if err != nil {
		return err
	}
	// every stored record lies inside a range the audit log announced (the log is
	// the authority on ids): the counter derived from the data must not be above
	// the first id the audit log has not announced
	if next > 1 && next > audited {
		return fmt.Errorf("%w: records up to id %d are stored but the audit log announced ids only below %d (run: minutiae case verify --case %s)",
			evidence.ErrIntegrity, next-1, audited, w.c.Dir)
	}
	stored := next
	next = max(next, audited, 1)
	if next > maxRecordID {
		return fmt.Errorf("%w: the next record id would be %d, above the cap of %d", evidence.ErrIntegrity, next, int64(maxRecordID))
	}

	hash := ""
	if w.parserHash != nil {
		hash = *w.parserHash
	}
	if _, err := w.c.Audit.Append(evidence.ActionIngestStart, "", evidence.IngestStart{
		IngestID: ingestID, Parser: w.p.Name, ParserVersion: w.p.Version, ParserHash: hash,
		AnalysisID: so.AnalysisID, Artifacts: artifacts, BatchRows: w.opt.BatchRows, Reingest: reingest,
		NormVersion: evidence.FTSNormVersion(),
	}.Details()); err != nil {
		return fmt.Errorf("audit ingest start: %w", err)
	}
	if err := w.callHook("after-start-audit"); err != nil {
		return err
	}
	err = w.c.StoreTx(ctx, func(tx *sql.Tx) error { return ensureParser(ctx, tx, w.p.Name, w.p.Version, w.parserHash) })
	if err != nil {
		return err
	}
	w.analysisID, w.declared, w.known, w.next, w.dbNext = so.AnalysisID, declared, known, next, stored
	w.ingest.Store(&ingestID)
	w.state = stateStarted
	return nil
}

// preflight runs the read-only checks before the start entry: the artifacts
// exist in artifacts.db, no complete run of this parser name+version covers one
// of them (unless allowed), the parser identity matches what is recorded. It
// returns whether an existing run was overridden and the next id the stored data
// implies: max(records.id)+1 (1 when there are no records), which records_meta.next_id
// must equal (storedNextID).
func (w *Writer) preflight(ctx context.Context, artifacts []string, allowReingest bool) (reingest bool, next int64, err error) {
	var covered []string
	err = w.c.ReadRecordsTx(ctx, func(h evidence.ReadHandle) error {
		for _, id := range artifacts {
			var n int
			if err := h.QueryRowContext(ctx, `SELECT count(*) FROM artifacts WHERE id = ?`, id).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("%w: %q is not in artifacts.db", ErrUnknownArtifact, clip(id))
			}
			ok, err := ingestedBy(ctx, h, id, w.p)
			if err != nil {
				return err
			}
			if ok {
				covered = append(covered, id)
			}
		}
		var stored sql.NullString
		err := h.QueryRowContext(ctx, `SELECT hash FROM parsers WHERE name = ? AND version = ?`, w.p.Name, w.p.Version).Scan(&stored)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case !sameHash(stored, w.parserHash):
			return fmt.Errorf("%w: %s %s", ErrParserIdentityConflict, clip(w.p.Name), clip(w.p.Version))
		}
		next, err = storedNextID(ctx, h)
		return err
	})
	if err != nil {
		return false, 0, err
	}
	if len(covered) > 0 && !allowReingest {
		return false, 0, fmt.Errorf("%w: %s %s already has a complete run over %q (use AllowReingest to ingest again)",
			ErrAlreadyIngested, clip(w.p.Name), clip(w.p.Version), clip(covered[0]))
	}
	return len(covered) > 0, next, nil
}

// auditedHighWater is the first id above every range any records.batch audit
// entry announced, whether or not its rows were written: ids are never reused.
func auditedHighWater(c *evidence.Case) (int64, error) {
	entries, err := c.ReadAudit()
	if err != nil {
		return 0, err
	}
	var hw int64
	for _, e := range entries {
		if e.Action != evidence.ActionBatch {
			continue
		}
		b, err := evidence.DecodeDetails[evidence.BatchCommit](e.Details)
		if err != nil {
			return 0, fmt.Errorf("%w: audit seq %d: %s: %w", evidence.ErrIntegrity, e.Seq, e.Action, err)
		}
		if b.FirstID < 1 || b.Count < 0 || b.FirstID > math.MaxInt64-int64(b.Count) {
			return 0, fmt.Errorf("%w: audit seq %d: batch range %d+%d is not valid", evidence.ErrIntegrity, e.Seq, b.FirstID, b.Count)
		}
		hw = max(hw, b.FirstID+int64(b.Count))
	}
	return hw, nil
}

// ingestedBy reports whether a complete run of parser p (name and version)
// covers artifactID.
func ingestedBy(ctx context.Context, h evidence.ReadHandle, artifactID string, p Parser) (bool, error) {
	var n int
	err := h.QueryRowContext(ctx, `SELECT count(*) FROM record_runs r
		JOIN parsers p ON p.id = r.parser_id
		JOIN record_run_artifacts a ON a.ingest_id = r.ingest_id
		WHERE r.outcome = 'complete' AND p.name = ? AND p.version = ? AND a.artifact_id = ?`,
		p.Name, p.Version, artifactID).Scan(&n)
	return n > 0, err
}

// AlreadyIngested reports whether a complete run of parser p (same name and
// version) covers the artifact. An incomplete or interrupted run does not count.
// It reads the database only: an ingest that concluded in the audit log but was
// never recorded is recovered by the next Writer.Start.
func AlreadyIngested(ctx context.Context, c *evidence.Case, artifactID string, p Parser) (bool, error) {
	if err := c.RequireSchema(3); err != nil {
		return false, err
	}
	var ok bool
	err := c.ReadRecordsTx(ctx, func(h evidence.ReadHandle) error {
		var err error
		ok, err = ingestedBy(ctx, h, artifactID, p)
		return err
	})
	return ok, err
}

// presentBatch is a record_batches row of an ingest being recovered.
type presentBatch struct {
	digest string
	first  int64
	count  int64
}

// recoverIngest resolves one ingest that has no run row. The records.ingest.recover
// entry is audited first (unless an earlier recovery already audited it) and the
// run row written after it. Nothing existing is edited or deleted.
func (w *Writer) recoverIngest(ctx context.Context, byID string, u evidence.UnresolvedIngest) error {
	var batches []presentBatch
	types := map[string]int64{}
	err := w.c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		rows, err := h.QueryContext(ctx, `SELECT digest, first_id, count FROM record_batches WHERE ingest_id = ? ORDER BY batch_no`, u.Start.IngestID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var b presentBatch
			if err := rows.Scan(&b.digest, &b.first, &b.count); err != nil {
				return err
			}
			batches = append(batches, b)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		trows, err := h.QueryContext(ctx, `SELECT r.type, count(*) FROM records r
			JOIN record_batches b ON b.batch_id = r.batch_id WHERE b.ingest_id = ? GROUP BY r.type`, u.Start.IngestID)
		if err != nil {
			return err
		}
		defer func() { _ = trows.Close() }()
		for trows.Next() {
			var t string
			var n int64
			if err := trows.Scan(&t, &n); err != nil {
				return err
			}
			types[t] = n
		}
		return trows.Err()
	})
	if err != nil {
		return err
	}

	var concl evidence.IngestConclusion
	var endSeq int64
	var ended string
	switch {
	case u.Kind == evidence.IngestRunMissing && u.Conclusion != nil:
		concl, endSeq, ended = *u.Conclusion, u.ConclusionSeq, u.ConclusionTime
	case u.Recover != nil:
		concl, endSeq, ended = u.Recover.IngestConclusion, u.RecoverSeq, u.RecoverTime
	default:
		concl = evidence.IngestConclusion{
			IngestID: u.Start.IngestID, Outcome: "interrupted", Batches: len(batches), Types: types,
			Rollup: evidence.IngestRollup(digestsOf(batches)),
			// what the audit log proves, never zeros (the suppressed numbers come from the conclusion note; an ingest that began suppressing and died before it is recovered with SuppressionUnknown set, never a silent 0)
			Warnings: u.Warnings, WarningsSuppressed: u.WarningsSuppressed, Rejected: u.Rejected,
			SuppressionUnknown: u.SuppressionUnknown,
		}
		for i, b := range batches {
			concl.Records += b.count
			if i == 0 {
				concl.FirstID = b.first
			}
			concl.FirstID = min(concl.FirstID, b.first)
			concl.LastID = max(concl.LastID, b.first+b.count-1)
		}
	}
	if u.Recover == nil {
		batchNos := u.AbsentBatches
		if batchNos == nil {
			batchNos = []int{}
		}
		rec := evidence.IngestRecover{
			IngestConclusion: concl, ByIngestID: byID, BatchNos: batchNos,
			RunMissing: u.Kind == evidence.IngestRunMissing,
		}
		if rec.RunMissing {
			rec.Reason = "the ingest concluded in the audit log but its run row was never written"
		} else {
			rec.Reason = "the ingest never concluded (the process stopped before records.ingest.end, or its abort could not be audited)"
		}
		e, err := w.c.Audit.Append(evidence.ActionIngestRecover, "", rec.Details())
		if err != nil {
			return fmt.Errorf("audit recover: %w", err)
		}
		if u.Kind != evidence.IngestRunMissing {
			endSeq, ended = e.Seq, e.Time
		}
	}
	if concl.Types == nil {
		concl.Types = map[string]int64{}
	}

	var hash *string
	if u.Start.ParserHash != "" {
		h := u.Start.ParserHash
		hash = &h
	}
	run := runRow{
		endSeq: endSeq, ended: ended, parserName: u.Start.Parser, parserVersion: u.Start.ParserVersion, parserHash: hash,
		analysisID: u.Start.AnalysisID, conclusion: concl, artifacts: u.Start.Artifacts,
	}
	return w.c.StoreTx(ctx, func(tx *sql.Tx) error { return insertRun(ctx, tx, run) })
}

func digestsOf(bs []presentBatch) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.digest
	}
	return out
}

// maxRecordID bounds every record id. Ids are int64 and a batch adds at most
// maxBatchRows to them; the cap keeps first+count far from overflowing whatever a
// tampered counter or audit range says.
const maxRecordID = 1 << 62

// storedNextID derives the next record id from the data: max(records.id)+1, or 1
// when there are no records. records_meta.next_id is an unaudited, mutable value
// an attacker can raise (the writer would then announce a batch range that was
// never used, or overflow, in the append-only audit log), so it is never
// trusted: it must equal the derived value, otherwise the case is inconsistent
// (ErrIntegrity) and the caller audits and writes nothing. Ids are only ever used
// above everything the audit log announced; Start takes the maximum of the two.
func storedNextID(ctx context.Context, h evidence.ReadHandle) (int64, error) {
	var meta string
	if err := h.QueryRowContext(ctx, `SELECT value FROM records_meta WHERE key = 'next_id'`).Scan(&meta); err != nil {
		return 0, err
	}
	var maxID int64
	if err := h.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM records`).Scan(&maxID); err != nil {
		return 0, err
	}
	if maxID < 0 || maxID >= maxRecordID {
		return 0, fmt.Errorf("%w: the highest record id is %d", evidence.ErrIntegrity, maxID)
	}
	derived := maxID + 1
	metaNext, perr := strconv.ParseInt(meta, 10, 64)
	if perr != nil || metaNext != derived {
		return 0, fmt.Errorf("%w: records_meta next_id is %q but the highest record id is %d: next_id must equal max(id)+1 (1 when there are no records); run: minutiae case verify",
			evidence.ErrIntegrity, clip(meta), maxID)
	}
	return derived, nil
}
