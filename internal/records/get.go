package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Get returns the record with that id with everything stored for it: body,
// payload (the canonical bytes exactly as stored), secondary times, the batch
// and run it belongs to, the artifact's manifest record and the run that
// superseded it, if any. ErrNotFound when there is no such record.
//
// A record whose artifact id is not exactly one manifest record is an integrity
// error (evidence.ErrIntegrity, exit 4; one that is in no manifest record also
// wraps evidence.ErrUnknownArtifact): `case verify` reports the details. A
// missing, empty, corrupt or torn audit log is an integrity error too; a plain
// I/O error reading it stays a plain error.
//
// The manifest and the audit log are read after the database transaction has
// closed, so they can differ in time from the row read. That is safe: the case
// lock excludes other processes, and an ingest in this process between the two
// reads can only add artifacts and audit entries, which the chain of an already
// stored record never needs.
func (r *Reader) Get(ctx context.Context, id int64) (Full, error) {
	var full Full
	err := r.c.ReadRecordsTx(ctx, func(h evidence.ReadHandle) error {
		have, err := hasSuperseded(ctx, h)
		if err != nil {
			return err
		}
		var body sql.NullString
		var payload string
		q := "SELECT " + rowColumns + ", " + supersededColumn(have) + ", r.body, r.payload" +
			fromRecords + joinParsers + joinBatches + " WHERE r.id = ?"
		row, err := scanRow(h.QueryRowContext(ctx, q, id), &body, &payload)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: id %d", ErrNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("records: get %d: %w", id, err)
		}
		full.Row, full.Body, full.Payload = row, body.String, json.RawMessage(payload)
		if full.Times, err = recordTimes(ctx, h, id); err != nil {
			return err
		}
		if full.Batch, err = batchOf(ctx, h, id); err != nil {
			return err
		}
		if full.Run, err = runOf(ctx, h, row.IngestID); err != nil {
			return err
		}
		var by sql.NullString
		err = h.QueryRowContext(ctx, evidence.SupersededBySQL, row.IngestID, row.ArtifactID).Scan(&by)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("records: superseded-by of %d: %w", id, err)
		}
		full.SupersededBy = by.String
		return nil
	})
	if err != nil {
		return Full{}, r.integrity(err)
	}
	man, err := r.c.Manifest()
	if err != nil {
		return Full{}, fmt.Errorf("records: record %d: manifest unreadable: %w", id, err)
	}
	art, err := artifactByID(man, full.ArtifactID)
	if err != nil {
		return Full{}, fmt.Errorf("records: record %d: %w", id, err)
	}
	full.Artifact, full.ArtifactIncomplete = art, art.Incomplete
	entries, err := r.c.ReadAudit()
	if err != nil {
		// a corrupt or torn log wraps evidence.ErrIntegrity (exit 4); an I/O error stays plain
		return Full{}, fmt.Errorf("records: %w", err)
	}
	if len(entries) == 0 {
		// a record exists, so the case was created with an audit log: a missing or empty one is damage
		return Full{}, fmt.Errorf("%w: records: record %d: the audit log is missing or empty; run: minutiae case verify --case %s", evidence.ErrIntegrity, id, r.c.Dir)
	}
	full.Batch.AuditSeq = batchAuditSeq(entries, full.Batch)
	full.Provenance = resolveChain(man, evidence.NewAuditIndex(entries), full.ArtifactID)
	byID := make(map[string]evidence.ManifestRecord, len(man))
	for _, m := range man {
		if _, dup := byID[m.ID]; !dup {
			byID[m.ID] = m // a duplicate id is reported by the chain, the first record is shown
		}
	}
	r.resolveRecovery(&full.Provenance, full.Row, byID)
	if len(full.Provenance.Chain) > 0 && full.Provenance.Chain[0].Artifact.Source.Derived == nil {
		full.Provenance.Notes = append(full.Provenance.Notes, "artifact is not derived: no image offset")
	}
	r.resolveOffsets(&full.Provenance, evidence.NewManifestIndex(man), full.Range)
	return full, nil
}

func recordTimes(ctx context.Context, h evidence.ReadHandle, id int64) ([]evidence.RecordTime, error) {
	rows, err := h.QueryContext(ctx, `SELECT kind, ts, ts_basis, tz_offset_min FROM record_times WHERE record_id = ? ORDER BY kind`, id)
	if err != nil {
		return nil, fmt.Errorf("records: times of %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []evidence.RecordTime
	for rows.Next() {
		var t evidence.RecordTime
		var tz sql.NullInt64
		if err := rows.Scan(&t.Kind, &t.TS, &t.Basis, &tz); err != nil {
			return nil, fmt.Errorf("records: times of %d: %w", id, err)
		}
		if tz.Valid {
			t.TZOffsetMin = &tz.Int64
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("records: times of %d: %w", id, err)
	}
	return out, nil
}

// batchOf reads the batch row of record id (zero when the row is missing).
func batchOf(ctx context.Context, h evidence.ReadHandle, id int64) (BatchInfo, error) {
	var b BatchInfo
	err := h.QueryRowContext(ctx, `SELECT b.ingest_id, b.batch_no, b.digest, b.first_id, b.count, b.created
		FROM records r JOIN record_batches b ON b.batch_id = r.batch_id WHERE r.id = ?`, id).
		Scan(&b.IngestID, &b.BatchNo, &b.Digest, &b.FirstID, &b.Count, &b.Created)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BatchInfo{}, fmt.Errorf("records: batch of %d: %w", id, err)
	}
	return b, nil
}

// runOf reads the run row of an ingest (nil when there is none).
func runOf(ctx context.Context, h evidence.ReadHandle, ingestID string) (*RunInfo, error) {
	if ingestID == "" {
		return nil, nil
	}
	var run RunInfo
	var analysis, hash sql.NullString
	err := h.QueryRowContext(ctx, `SELECT ru.end_seq, ru.ingest_id, ru.analysis_id, p.name, p.version, p.hash, ru.outcome,
			ru.batches, ru.records, ru.first_id, ru.last_id, ru.ended
		FROM record_runs ru LEFT JOIN parsers p ON p.id = ru.parser_id WHERE ru.ingest_id = ?`, ingestID).
		Scan(&run.ID, &run.IngestID, &analysis, &run.Parser, &run.ParserVersion, &hash, &run.Outcome,
			&run.Batches, &run.Records, &run.FirstID, &run.LastID, &run.Ended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("records: run %q: %w", ingestID, err)
	}
	run.AnalysisID, run.ParserHash, run.AuditSeq = analysis.String, hash.String, run.ID
	return &run, nil
}

// batchAuditSeq finds the audit sequence of the records.batch entry of b: 0
// when the batch row is missing or the log holds no such entry (verify reports
// that; Get only shows provenance).
func batchAuditSeq(entries []evidence.AuditEntry, b BatchInfo) int64 {
	if b.IngestID == "" {
		return 0
	}
	for _, e := range entries {
		if e.Action != evidence.ActionBatch {
			continue
		}
		d, err := evidence.DecodeDetails[evidence.BatchCommit](e.Details)
		if err == nil && d.IngestID == b.IngestID && d.BatchNo == b.BatchNo {
			return e.Seq
		}
	}
	return 0
}

// artifactByID resolves id as an artifact id and nothing else: no fallback to a
// manifest path (as Case.FindArtifact has), so a record whose artifact_id was
// rewritten to another artifact's path cannot silently show that artifact. No
// match is evidence.ErrIntegrity wrapping evidence.ErrUnknownArtifact; more than
// one is evidence.ErrIntegrity.
func artifactByID(recs []evidence.ManifestRecord, id string) (evidence.ManifestRecord, error) {
	var found evidence.ManifestRecord
	n := 0
	for _, m := range recs {
		if m.ID == id {
			found = m
			n++
		}
	}
	switch {
	case n == 1:
		return found, nil
	case n > 1:
		return evidence.ManifestRecord{}, fmt.Errorf("%w: artifact id %q appears %d times in the manifest", evidence.ErrIntegrity, id, n)
	}
	// the database says a record came from this artifact and the manifest has no such
	// artifact: the case is inconsistent (verify P1), not a bad request
	return evidence.ManifestRecord{}, fmt.Errorf("%w: %w: %q", evidence.ErrIntegrity, evidence.ErrUnknownArtifact, id)
}
