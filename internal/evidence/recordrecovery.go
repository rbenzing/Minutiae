package evidence

import (
	"context"
	"errors"
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
		SELECT 1 FROM ` + supersedingRunFrom + `
		WHERE ` + supersedingRunWhere + `)`

// supersedingRunFrom and supersedingRunWhere are the two halves of the
// definition: a run n (parser np, coverage na) supersedes the run o (parser op,
// coverage oa) for the artifact oa covers. SupersededPairsSQL and
// SupersededBySQL are both built from them, so there is one definition.
const (
	supersedingRunFrom = `record_runs n
		JOIN parsers np ON np.id = n.parser_id
		JOIN record_run_artifacts na ON na.ingest_id = n.ingest_id`
	supersedingRunWhere = `n.outcome = 'complete' AND np.name = op.name
			AND na.artifact_id = oa.artifact_id AND n.end_seq > o.end_seq`
)

// SupersededBySQL selects, for the run (ingest_id = ?1) and artifact
// (artifact_id = ?2), the ingest id of the lowest-ending run that supersedes it
// (at most one row; none when the run is current for that artifact). It is built
// from the same definition as SupersededPairsSQL.
const SupersededBySQL = `SELECT n.ingest_id
	FROM record_runs o
	JOIN parsers op ON op.id = o.parser_id
	JOIN record_run_artifacts oa ON oa.ingest_id = o.ingest_id
	JOIN ` + supersedingRunFrom + `
	WHERE o.ingest_id = ?1 AND oa.artifact_id = ?2 AND ` + supersedingRunWhere + `
	ORDER BY n.end_seq LIMIT 1`

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

	// Warnings, WarningsSuppressed and Rejected are what the audit log proves about the ingest's
	// analysis.warning entries: the entries (the suppression notes are not entries), the entries the cap kept out
	// of the log and the rejections, both as the conclusion note states them. An ingest that wrote its suppression
	// began note but died before the conclusion note has no provable suppressed entries: SuppressionUnknown is
	// set, WarningsSuppressed is 0 and Rejected holds the rejection entries only (lower bounds). Recovery states
	// these numbers and the flag instead of silent zeros.
	Warnings           int
	WarningsSuppressed int
	Rejected           int
	// SuppressionUnknown: the ingest wrote its suppression began note but no conclusion note, so the suppressed
	// numbers are unknown (WarningsSuppressed and the suppressed part of Rejected are lower bounds).
	SuppressionUnknown bool
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
		tally      IngestWarningTally
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
		case ActionAnalysisWarning:
			if in := byID[writerWarningIngest(e.Details)]; in != nil {
				in.tally.add(e.Details)
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
		u := UnresolvedIngest{
			Start: in.start, StartSeq: in.startSeq, Kind: IngestUnfinished,
			Warnings: in.tally.Warnings, WarningsSuppressed: in.tally.Suppressed(), Rejected: in.tally.ProvenRejected(),
			SuppressionUnknown: in.tally.Began > 0 && in.tally.Notes == 0,
		}
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

// ErrIngestActive is returned when the Case's live-ingest slot is taken: by a records writer that
// was started and neither ended nor aborted, or by a running records reindex (BeginIngest).
var ErrIngestActive = errors.New("an ingest is already active in this case")

// BeginIngest registers id as the live ingest of this Case. It refuses (ok is
// false, active names the ingest) while another ingest registered earlier is
// still live: one Case has at most one live ingest in this process, so recovery
// can never mistake a running ingest for a dead one. The caller must call
// EndIngest when the ingest concludes or its Start fails.
func (c *Case) BeginIngest(id string) (active string, ok bool) {
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	if c.liveIngest != "" {
		return c.liveIngest, false
	}
	c.liveIngest = id
	return "", true
}

// EndIngest releases the registration BeginIngest made for id (a no-op for any
// other id).
func (c *Case) EndIngest(id string) {
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	if c.liveIngest == id {
		c.liveIngest = ""
	}
}

// LiveIngest returns the id of the ingest live in this Case ("" when none).
func (c *Case) LiveIngest() string {
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	return c.liveIngest
}
