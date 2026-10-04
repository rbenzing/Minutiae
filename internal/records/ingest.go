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

// Start begins the ingest, in this order: it checks the schema and the options,
// reads the manifest, RECOVERS every ingest the audit log announced and the
// database never concluded (see below), refuses a parser name+version that a
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
	if err := w.c.RequireSchema(2); err != nil {
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
	w.ingest.Store(&ingestID)

	unresolved, err := w.c.UnresolvedIngests()
	if err != nil {
		return err
	}
	for _, u := range unresolved {
		if err := w.recoverIngest(ctx, u); err != nil {
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
	next = max(next, audited, 1)

	hash := ""
	if w.parserHash != nil {
		hash = *w.parserHash
	}
	if _, err := w.c.Audit.Append(evidence.ActionIngestStart, "", evidence.IngestStart{
		IngestID: ingestID, Parser: w.p.Name, ParserVersion: w.p.Version, ParserHash: hash,
		AnalysisID: so.AnalysisID, Artifacts: artifacts, BatchRows: w.opt.BatchRows, Reingest: reingest,
	}.Details()); err != nil {
		return fmt.Errorf("audit ingest start: %w", err)
	}
	err = w.c.StoreTx(ctx, func(tx *sql.Tx) error { return ensureParser(ctx, tx, w.p.Name, w.p.Version, w.parserHash) })
	if err != nil {
		return err
	}
	w.analysisID, w.declared, w.known, w.next = so.AnalysisID, declared, known, next
	w.state = stateStarted
	return nil
}

// preflight runs the read-only checks before the start entry: the artifacts
// exist in artifacts.db, no complete run of this parser name+version covers one
// of them (unless allowed), the parser identity matches what is recorded. It
// returns whether an existing run was overridden and the next id the database
// itself implies (records_meta.next_id and max(records.id)+1).
func (w *Writer) preflight(ctx context.Context, artifacts []string, allowReingest bool) (reingest bool, next int64, err error) {
	var covered []string
	err = w.c.ReadTx(ctx, func(h evidence.ReadHandle) error {
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
		var meta string
		if err := h.QueryRowContext(ctx, `SELECT value FROM records_meta WHERE key = 'next_id'`).Scan(&meta); err != nil {
			return err
		}
		metaNext, perr := strconv.ParseInt(meta, 10, 64)
		if perr != nil || metaNext < 1 {
			return fmt.Errorf("%w: records_meta next_id is %q", evidence.ErrIntegrity, clip(meta))
		}
		var maxID int64
		if err := h.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM records`).Scan(&maxID); err != nil {
			return err
		}
		if maxID == math.MaxInt64 {
			return fmt.Errorf("%w: a record id is %d", evidence.ErrIntegrity, maxID)
		}
		next = max(metaNext, maxID+1)
		return nil
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
	if err := c.RequireSchema(2); err != nil {
		return false, err
	}
	var ok bool
	err := c.ReadTx(ctx, func(h evidence.ReadHandle) error {
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
func (w *Writer) recoverIngest(ctx context.Context, u evidence.UnresolvedIngest) error {
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
			IngestConclusion: concl, ByIngestID: w.IngestID(), BatchNos: batchNos,
			RunMissing: u.Kind == evidence.IngestRunMissing,
		}
		if rec.RunMissing {
			rec.Reason = "the ingest concluded in the audit log but its run row was never written"
		} else {
			rec.Reason = "the ingest never concluded (the process stopped before records.ingest.end)"
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
