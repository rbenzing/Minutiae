package evidence

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
)

// SupersededPairsSQL is the one definition of supersession, a SELECT of the
// pairs (ingest_id, artifact_id) a reader hides, the writer records in
// record_superseded and verify recomputes: run O is superseded for artifact A
// when some complete run N of the same parser name (any version) covers A and
// ended later (N.end_seq > O.end_seq). It is order-insensitive: the result does
// not depend on the order the runs were inserted in. Only a complete run
// supersedes (an incomplete or interrupted one never hides the results of a
// complete one), and any run can be superseded.
const SupersededPairsSQL = `SELECT DISTINCT o.ingest_id AS ingest_id, oa.artifact_id AS artifact_id
	FROM record_runs o
	JOIN parsers op ON op.id = o.parser_id
	JOIN record_run_artifacts oa ON oa.ingest_id = o.ingest_id
	WHERE EXISTS (
		SELECT 1 FROM record_runs n
		JOIN parsers np ON np.id = n.parser_id
		JOIN record_run_artifacts na ON na.ingest_id = n.ingest_id
		WHERE n.outcome = 'complete' AND np.name = op.name
			AND na.artifact_id = oa.artifact_id AND n.end_seq > o.end_seq)`

// Kinds of UnresolvedIngest.
const (
	// IngestUnfinished: records.ingest.start with no end or error entry.
	IngestUnfinished = "unfinished"
	// IngestRunMissing: a records.ingest.end/error entry whose run row was never written.
	IngestRunMissing = "run-missing"
)

// UnresolvedIngest is an ingest the audit log announced whose run row is not in
// the database: the process stopped (or the database write failed) before the
// ingest was concluded in the database.
type UnresolvedIngest struct {
	Start    IngestStart
	StartSeq int64
	Kind     string // IngestUnfinished | IngestRunMissing

	// Conclusion is the audited records.ingest.end/error of a run-missing ingest.
	Conclusion     *IngestConclusion
	ConclusionSeq  int64
	ConclusionTime string

	// Recover is a records.ingest.recover that was audited but whose run row was
	// not written (the process stopped in between); recovery then writes the row
	// without auditing a second recover.
	Recover     *IngestRecover
	RecoverSeq  int64
	RecoverTime string

	// AbsentBatches are the announced batches that have no batch row, no
	// records.batch.error entry and are not named by a recover: audited, never
	// written.
	AbsentBatches []int
}

// UnresolvedIngests lists, in start order, the ingests the audit log announced
// that have no run row. It reads the audit log and the database (nothing is
// written); a case still at schema v1 has none. Writer.Start recovers each one
// before it starts a new ingest, and verify reports them.
func (c *Case) UnresolvedIngests() ([]UnresolvedIngest, error) {
	if v, err := c.store.SchemaVersion(); err != nil {
		return nil, fmt.Errorf("artifacts.db schema_version: %w", err)
	} else if v < 2 {
		return nil, nil
	}
	entries, err := c.ReadAudit()
	if err != nil {
		return nil, err
	}

	type ingest struct {
		start      IngestStart
		startSeq   int64
		announced  []int
		errored    map[int]bool
		conclusion *IngestConclusion
		concSeq    int64
		concTime   string
		recover    *IngestRecover
		recSeq     int64
		recTime    string
	}
	var order []*ingest
	byID := map[string]*ingest{}
	for _, e := range entries {
		switch e.Action {
		case ActionIngestStart:
			s, err := DecodeDetails[IngestStart](e.Details)
			if err != nil {
				return nil, fmt.Errorf("%w: audit seq %d: %s: %w", ErrIntegrity, e.Seq, e.Action, err)
			}
			if byID[s.IngestID] == nil {
				in := &ingest{start: s, startSeq: e.Seq, errored: map[int]bool{}}
				byID[s.IngestID] = in
				order = append(order, in)
			}
		case ActionBatch:
			b, err := DecodeDetails[BatchCommit](e.Details)
			if err != nil {
				return nil, fmt.Errorf("%w: audit seq %d: %s: %w", ErrIntegrity, e.Seq, e.Action, err)
			}
			if in := byID[b.IngestID]; in != nil {
				in.announced = append(in.announced, b.BatchNo)
			}
		case ActionBatchError:
			b, err := DecodeDetails[BatchFailure](e.Details)
			if err != nil {
				return nil, fmt.Errorf("%w: audit seq %d: %s: %w", ErrIntegrity, e.Seq, e.Action, err)
			}
			if in := byID[b.IngestID]; in != nil {
				in.errored[b.BatchNo] = true
			}
		case ActionIngestEnd, ActionIngestError:
			cn, err := DecodeDetails[IngestConclusion](e.Details)
			if err != nil {
				return nil, fmt.Errorf("%w: audit seq %d: %s: %w", ErrIntegrity, e.Seq, e.Action, err)
			}
			if in := byID[cn.IngestID]; in != nil && in.conclusion == nil {
				in.conclusion, in.concSeq, in.concTime = &cn, e.Seq, e.Time
			}
		case ActionIngestRecover:
			r, err := DecodeDetails[IngestRecover](e.Details)
			if err != nil {
				return nil, fmt.Errorf("%w: audit seq %d: %s: %w", ErrIntegrity, e.Seq, e.Action, err)
			}
			if in := byID[r.IngestID]; in != nil && in.recover == nil {
				in.recover, in.recSeq, in.recTime = &r, e.Seq, e.Time
			}
		}
	}
	if len(order) == 0 {
		return nil, nil
	}

	runs := map[string]bool{}
	present := map[string]map[int]bool{}
	err = c.ReadTx(context.Background(), func(h ReadHandle) error {
		rows, err := h.Query(`SELECT ingest_id FROM record_runs`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			runs[id] = true
		}
		if err := rows.Err(); err != nil {
			return err
		}
		brows, err := h.Query(`SELECT ingest_id, batch_no FROM record_batches`)
		if err != nil {
			return err
		}
		defer func() { _ = brows.Close() }()
		for brows.Next() {
			var id string
			var no int
			if err := brows.Scan(&id, &no); err != nil {
				return err
			}
			if present[id] == nil {
				present[id] = map[int]bool{}
			}
			present[id][no] = true
		}
		return brows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("artifacts.db: %w", err)
	}

	var out []UnresolvedIngest
	for _, in := range order {
		if runs[in.start.IngestID] {
			continue
		}
		u := UnresolvedIngest{Start: in.start, StartSeq: in.startSeq, Kind: IngestUnfinished}
		if in.conclusion != nil {
			u.Kind, u.Conclusion, u.ConclusionSeq, u.ConclusionTime = IngestRunMissing, in.conclusion, in.concSeq, in.concTime
		}
		if in.recover != nil {
			u.Recover, u.RecoverSeq, u.RecoverTime = in.recover, in.recSeq, in.recTime
		}
		named := map[int]bool{}
		if in.recover != nil {
			for _, n := range in.recover.BatchNos {
				named[n] = true
			}
		}
		for _, n := range in.announced {
			if !present[in.start.IngestID][n] && !in.errored[n] && !named[n] {
				u.AbsentBatches = append(u.AbsentBatches, n)
			}
		}
		sort.Ints(u.AbsentBatches)
		out = append(out, u)
	}
	return out, nil
}

// ReadAudit returns every entry of the case's audit log.
func (c *Case) ReadAudit() ([]AuditEntry, error) {
	entries, err := ReadAuditEntries(filepath.Join(c.Dir, auditFile))
	if err != nil {
		return nil, fmt.Errorf("read audit log: %w", err)
	}
	return entries, nil
}
