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
// A record whose artifact cannot be resolved in the manifest is an error
// (wrapping evidence.ErrUnknownArtifact or evidence.ErrIntegrity): `case verify`
// reports the details.
func (r *Reader) Get(ctx context.Context, id int64) (Full, error) {
	var full Full
	err := r.c.ReadTx(ctx, func(h evidence.ReadHandle) error {
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
		return Full{}, err
	}
	art, err := r.c.FindArtifact(full.ArtifactID)
	if err != nil {
		return Full{}, fmt.Errorf("records: record %d: %w", id, err)
	}
	full.Artifact, full.ArtifactIncomplete = art, art.Incomplete
	if full.Batch.AuditSeq, err = r.batchAuditSeq(full.Batch); err != nil {
		return Full{}, err
	}
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
func (r *Reader) batchAuditSeq(b BatchInfo) (int64, error) {
	if b.IngestID == "" {
		return 0, nil
	}
	entries, err := r.c.ReadAudit()
	if err != nil {
		return 0, fmt.Errorf("records: %w", err)
	}
	for _, e := range entries {
		if e.Action != evidence.ActionBatch {
			continue
		}
		d, err := evidence.DecodeDetails[evidence.BatchCommit](e.Details)
		if err == nil && d.IngestID == b.IngestID && d.BatchNo == b.BatchNo {
			return e.Seq, nil
		}
	}
	return 0, nil
}
