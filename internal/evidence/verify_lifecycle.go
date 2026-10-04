package evidence

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// Ingest lifecycle verification (P6), run rows and their coverage (P14),
// supersession (P14) and the artifact hashes the batch entries carry (P7). The
// audit log is the authority: every ingest announces itself with
// records.ingest.start and concludes with exactly one records.ingest.end or
// .error, or, when the process died, with a records.ingest.recover written by
// the next ingest; the database's run rows must equal what that entry says.

// auditedConclusion is a records.ingest.end or records.ingest.error entry.
type auditedConclusion struct {
	Seq    int64
	Action string
	Time   string
	IngestConclusion
}

// auditedRecover is a records.ingest.recover entry.
type auditedRecover struct {
	Seq  int64
	Time string
	IngestRecover
}

// expectedRun is the run row the audit log demands for one ingest.
type expectedRun struct {
	start  IngestStart
	endSeq int64
	ended  string
	concl  IngestConclusion
	cover  map[string]bool // start.Artifacts as a set
}

// storedRun is a record_runs row, with its parser joined (hasParser is false when
// the parsers row is gone).
type storedRun struct {
	endSeq        int64
	ingest        string
	parserID      int64
	hasParser     bool
	pName, pVer   string
	pHash         sql.NullString
	analysis      sql.NullString
	outcome       string
	batches       int64
	records       int64
	firstID       int64
	lastID        int64
	rollup, ended string
}

// sameConclusion compares the totals two conclusion entries carry (not the
// per-type counts).
func sameConclusion(a, b IngestConclusion) bool {
	return a.Outcome == b.Outcome && a.Batches == b.Batches && a.Records == b.Records &&
		a.FirstID == b.FirstID && a.LastID == b.LastID && a.Rollup == b.Rollup && a.Error == b.Error
}

// verifyLifecycle runs P6, P7 and P14. byKey and batchesRead describe the stored
// record_batches rows: an audited batch counts toward its ingest's totals when
// its row exists, or when nothing explains its absence (records.batch.error or a
// recover entry naming it) - an erased row is reported as such, not as a second
// problem in the totals.
func (c *Case) verifyLifecycle(ctx context.Context, rep *VerifyReport, ps *problemSet, audit *recordAudit, manifest map[string]ManifestRecord, byKey map[batchKey]storedBatch, batchesRead bool) {
	checkBatchArtifacts(ps, audit, manifest)

	byIngest := map[string][]auditedBatch{}
	for _, b := range audit.batches {
		byIngest[b.IngestID] = append(byIngest[b.IngestID], b)
		if _, ok := audit.starts[b.IngestID]; !ok {
			ps.add("lifecycle", "batch %d of ingest %q has no records.ingest.start audit entry", b.BatchNo, b.IngestID)
		}
	}
	live := c.LiveIngest()
	expected := map[string]expectedRun{}
	for _, id := range audit.startOrder {
		if exp, ok := c.checkIngest(rep, ps, audit, id, live, byIngest[id], byKey, batchesRead); ok {
			expected[id] = exp
		}
	}
	for _, id := range audit.strayIngests() {
		ps.add("lifecycle", "ingest %q has a records.ingest.end, .error or .recover audit entry but no records.ingest.start", id)
	}

	runs, runsOK := c.checkRuns(ctx, rep, ps, expected)
	if runsOK {
		c.checkCoverage(ctx, rep, ps, expected, runs)
	}
	c.checkSupersession(ctx, rep, ps)
}

// checkBatchArtifacts (P7): the SHA-256 each records.batch entry commits to for
// its artifacts must be the one the manifest holds.
func checkBatchArtifacts(ps *problemSet, audit *recordAudit, manifest map[string]ManifestRecord) {
	for _, b := range audit.batches {
		ids := make([]string, 0, len(b.Artifacts))
		for id := range b.Artifacts {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			m, ok := manifest[id]
			switch {
			case !ok:
				ps.add("batch-artifact", "batch %d of ingest %q: artifact %q is not in the manifest", b.BatchNo, b.IngestID, id)
			case m.SHA256 != b.Artifacts[id]:
				ps.add("batch-artifact", "batch %d of ingest %q: artifact %q has sha256 %q in the audit log, which differs from the manifest (%q)",
					b.BatchNo, b.IngestID, id, b.Artifacts[id], m.SHA256)
			}
		}
	}
}

// strayIngests lists, sorted, the ingest ids that have a conclusion or recover
// entry but no start entry.
func (a *recordAudit) strayIngests() []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if _, started := a.starts[id]; !started && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for id := range a.ends {
		add(id)
	}
	for id := range a.recovers {
		add(id)
	}
	sort.Strings(out)
	return out
}

// checkIngest applies the lifecycle rules to one started ingest and returns the
// run row the audit log demands (ok is false when it demands none: the ingest
// never concluded or its entries contradict each other).
func (c *Case) checkIngest(rep *VerifyReport, ps *problemSet, audit *recordAudit, id, live string, batches []auditedBatch, byKey map[batchKey]storedBatch, batchesRead bool) (expectedRun, bool) {
	st := audit.starts[id]
	startSeq := audit.startSeq[id]
	if audit.dupStart[id] {
		ps.add("lifecycle", "ingest %q: records.ingest.start is audited more than once", id)
	}
	ends, recs := audit.ends[id], audit.recovers[id]
	if len(ends) > 1 {
		ps.add("lifecycle", "ingest %q: concluded more than once (audit seq %d and %d)", id, ends[0].Seq, ends[1].Seq)
	}
	if len(recs) > 1 {
		ps.add("lifecycle", "ingest %q: recovered more than once (audit seq %d and %d)", id, recs[0].Seq, recs[1].Seq)
	}
	if len(ends) == 0 && len(recs) == 0 {
		if id == live {
			rep.noticef("ingest %q is still running in this process (records.ingest.start at audit seq %d has no conclusion yet)", id, startSeq)
			return expectedRun{}, false
		}
		ps.add("lifecycle", "ingest %q: interrupted: records.ingest.start (audit seq %d) has no records.ingest.end, .error or .recover entry", id, startSeq)
		return expectedRun{}, false
	}
	var en *auditedConclusion
	if len(ends) > 0 {
		en = &ends[0]
	}
	var rc *auditedRecover
	if len(recs) > 0 {
		rc = &recs[0]
		reason := "it never concluded"
		if rc.RunMissing {
			reason = "its run row had never been written"
		}
		rep.noticef("ingest %q was recovered by ingest %q (records.ingest.recover, audit seq %d): %s; announced batches that never reached the database: %v",
			id, rc.ByIngestID, rc.Seq, reason, rc.BatchNos)
	}
	okRules := true
	switch {
	case rc != nil && rc.RunMissing && en == nil:
		ps.add("lifecycle", "ingest %q: records.ingest.recover (audit seq %d) says its run row was missing, but the audit log holds no records.ingest.end or .error for it", id, rc.Seq)
		okRules = false
	case rc != nil && !rc.RunMissing && en != nil:
		ps.add("lifecycle", "ingest %q: records.ingest.recover (audit seq %d) says the ingest never concluded, but audit seq %d concluded it", id, rc.Seq, en.Seq)
	case rc != nil && !rc.RunMissing && rc.Outcome != "interrupted":
		ps.add("lifecycle", "ingest %q: records.ingest.recover (audit seq %d) of an unfinished ingest has outcome %q, not \"interrupted\"", id, rc.Seq, rc.Outcome)
	}
	if !okRules {
		return expectedRun{}, false
	}

	exp := expectedRun{start: st, cover: make(map[string]bool, len(st.Artifacts))}
	for _, a := range st.Artifacts {
		exp.cover[a] = true
	}
	what, seq := "", int64(0)
	switch {
	case en != nil:
		want := map[string]string{ActionIngestEnd: "complete", ActionIngestError: "incomplete"}[en.Action]
		if en.Outcome != want {
			ps.add("lifecycle", "ingest %q: %s (audit seq %d) has outcome %q, want %q", id, en.Action, en.Seq, en.Outcome, want)
		}
		exp.concl, exp.endSeq, exp.ended = en.IngestConclusion, en.Seq, en.Time
		what, seq = en.Action, en.Seq
		if rc != nil && rc.RunMissing && !sameConclusion(rc.IngestConclusion, en.IngestConclusion) {
			ps.add("lifecycle", "ingest %q: records.ingest.recover (audit seq %d) differs from the conclusion it copies (audit seq %d)", id, rc.Seq, en.Seq)
		}
	default:
		exp.concl, exp.endSeq, exp.ended = rc.IngestConclusion, rc.Seq, rc.Time
		what, seq = ActionIngestRecover, rc.Seq
	}
	if seq < startSeq {
		ps.add("lifecycle", "ingest %q: %s (audit seq %d) comes before its records.ingest.start (audit seq %d)", id, what, seq, startSeq)
	}
	if exp.concl.IngestID != id {
		ps.add("lifecycle", "ingest %q: %s (audit seq %d) names ingest %q", id, what, seq, exp.concl.IngestID)
	}
	c.checkTotals(ps, id, what, seq, exp.concl, committedBatches(audit, id, batches, byKey, batchesRead))
	return exp, true
}

// committedBatches returns the audited batches of an ingest that count toward its
// totals, in batch_no order: those with a stored row, and those whose absence
// nothing explains. A batch explained by records.batch.error or named by a
// recover entry never reached the database and is not part of the totals.
func committedBatches(audit *recordAudit, id string, batches []auditedBatch, byKey map[batchKey]storedBatch, batchesRead bool) []auditedBatch {
	var out []auditedBatch
	for _, b := range batches {
		k := batchKey{id, b.BatchNo}
		_, have := byKey[k]
		if have && batchesRead || (!audit.batchErr[k] && !audit.recovered[k]) {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BatchNo < out[j].BatchNo })
	return out
}

// checkTotals requires a conclusion's totals to equal those of the audited
// batches it covers.
func (c *Case) checkTotals(ps *problemSet, id, what string, seq int64, got IngestConclusion, batches []auditedBatch) {
	var records int64
	var first, last int64
	digests := make([]string, len(batches))
	for i, b := range batches {
		records += int64(b.Count)
		if i == 0 {
			first = b.FirstID
		}
		last = b.lastID()
		digests[i] = b.Digest
	}
	rollup := IngestRollup(digests)
	var diffs []string
	if got.Batches != len(batches) {
		diffs = append(diffs, fmt.Sprintf("batches is %d, the batches hold %d", got.Batches, len(batches)))
	}
	if got.Records != records {
		diffs = append(diffs, fmt.Sprintf("records is %d, the batches hold %d", got.Records, records))
	}
	if got.FirstID != first {
		diffs = append(diffs, fmt.Sprintf("first_id is %d, the batches start at %d", got.FirstID, first))
	}
	if got.LastID != last {
		diffs = append(diffs, fmt.Sprintf("last_id is %d, the batches end at %d", got.LastID, last))
	}
	if got.Rollup != rollup {
		diffs = append(diffs, fmt.Sprintf("rollup is %q, the batches hold %q", got.Rollup, rollup))
	}
	for _, d := range diffs {
		ps.add("totals", "ingest %q: %s (audit seq %d) does not match its audited batches: %s", id, what, seq, d)
	}
}

const runsSelect = `SELECT r.end_seq, r.ingest_id, r.parser_id, p.name, p.version, p.hash, r.analysis_id, r.outcome,
	r.batches, r.records, r.first_id, r.last_id, r.rollup, r.ended
	FROM record_runs r LEFT JOIN parsers p ON p.id = r.parser_id
	WHERE r.end_seq > ? ORDER BY r.end_seq LIMIT ?`

// checkRuns compares every record_runs row with the run the audit log demands and
// reports the demanded runs that have no row. It returns the ingests that have a
// row and whether the table could be read.
func (c *Case) checkRuns(ctx context.Context, rep *VerifyReport, ps *problemSet, expected map[string]expectedRun) (map[string]bool, bool) {
	have := map[string]bool{}
	after := int64(-1) << 62
	for {
		var chunk []storedRun
		err := c.ReadTx(ctx, func(h ReadHandle) error {
			chunk = nil
			rows, err := h.QueryContext(ctx, runsSelect, after, verifyChunkRows)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var r storedRun
				var name, ver sql.NullString
				if err := rows.Scan(&r.endSeq, &r.ingest, &r.parserID, &name, &ver, &r.pHash, &r.analysis, &r.outcome,
					&r.batches, &r.records, &r.firstID, &r.lastID, &r.rollup, &r.ended); err != nil {
					return err
				}
				r.hasParser, r.pName, r.pVer = name.Valid, name.String, ver.String
				chunk = append(chunk, r)
			}
			return rows.Err()
		})
		if err != nil {
			unreadable(rep, "record_runs", err)
			return have, false
		}
		for _, r := range chunk {
			rep.RecordRunsChecked++
			have[r.ingest] = true
			compareRun(ps, r, expected)
		}
		if len(chunk) < verifyChunkRows {
			break
		}
		after = chunk[len(chunk)-1].endSeq
	}
	ids := make([]string, 0, len(expected))
	for id := range expected {
		if !have[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := expected[id]
		ps.add("run-missing", "ingest %q: run row missing: the audit log concluded it at seq %d but record_runs has no row for it", id, e.endSeq)
	}
	return have, true
}

// compareRun reports every field of a run row that differs from the audit.
func compareRun(ps *problemSet, r storedRun, expected map[string]expectedRun) {
	exp, ok := expected[r.ingest]
	if !ok {
		ps.add("run", "run row of ingest %q has no matching audit entries: the audit log holds no records.ingest.start and a records.ingest.end, .error or .recover for it", r.ingest)
		return
	}
	differ := func(format string, a ...any) {
		ps.add("run", "run row of ingest %q differs from the audit log (audit seq %d): "+format, append([]any{r.ingest, exp.endSeq}, a...)...)
	}
	if r.endSeq != exp.endSeq {
		differ("end_seq is %d, audited %d", r.endSeq, exp.endSeq)
	}
	st := exp.start
	switch {
	case !r.hasParser:
		differ("parser id %d does not exist in parsers", r.parserID)
	case r.pName != st.Parser || r.pVer != st.ParserVersion || r.pHash.String != st.ParserHash:
		differ("parser is %q %q %q, audited %q %q %q", r.pName, r.pVer, r.pHash.String, st.Parser, st.ParserVersion, st.ParserHash)
	}
	if r.analysis.String != st.AnalysisID {
		differ("analysis_id is %q, audited %q", r.analysis.String, st.AnalysisID)
	}
	cn := exp.concl
	if r.outcome != cn.Outcome {
		differ("outcome is %q, audited %q", r.outcome, cn.Outcome)
	}
	if r.batches != int64(cn.Batches) {
		differ("batches is %d, audited %d", r.batches, cn.Batches)
	}
	if r.records != cn.Records {
		differ("records is %d, audited %d", r.records, cn.Records)
	}
	if r.firstID != cn.FirstID {
		differ("first_id is %d, audited %d", r.firstID, cn.FirstID)
	}
	if r.lastID != cn.LastID {
		differ("last_id is %d, audited %d", r.lastID, cn.LastID)
	}
	if r.rollup != cn.Rollup {
		differ("rollup is %q, audited %q", r.rollup, cn.Rollup)
	}
	if r.ended != exp.ended {
		differ("ended is %q, audited %q", r.ended, exp.ended)
	}
}

const (
	coverageFirst = `SELECT ingest_id, artifact_id FROM record_run_artifacts ORDER BY ingest_id, artifact_id LIMIT ?`
	coverageNext  = `SELECT ingest_id, artifact_id FROM record_run_artifacts WHERE (ingest_id, artifact_id) > (?, ?) ORDER BY ingest_id, artifact_id LIMIT ?`
)

// checkCoverage requires record_run_artifacts to hold, for each run row, exactly
// the artifacts of the ingest's records.ingest.start entry.
func (c *Case) checkCoverage(ctx context.Context, rep *VerifyReport, ps *problemSet, expected map[string]expectedRun, runs map[string]bool) {
	seen := map[string]map[string]bool{}
	var afterIngest, afterArtifact string
	first := true
	for {
		var chunk [][2]string
		err := c.ReadTx(ctx, func(h ReadHandle) error {
			chunk = nil
			var rows *sql.Rows
			var err error
			if first {
				rows, err = h.QueryContext(ctx, coverageFirst, verifyChunkRows)
			} else {
				rows, err = h.QueryContext(ctx, coverageNext, afterIngest, afterArtifact, verifyChunkRows)
			}
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var p [2]string
				if err := rows.Scan(&p[0], &p[1]); err != nil {
					return err
				}
				chunk = append(chunk, p)
			}
			return rows.Err()
		})
		if err != nil {
			unreadable(rep, "record_run_artifacts", err)
			return
		}
		for _, p := range chunk {
			ing, art := p[0], p[1]
			exp, demanded := expected[ing]
			switch {
			case !runs[ing]:
				ps.add("coverage", "record_run_artifacts row (ingest %q, artifact %q) belongs to no run row", ing, art)
			case !demanded:
				// the run row itself is reported
			case !exp.cover[art]:
				ps.add("coverage", "run coverage of ingest %q: artifact %q is in record_run_artifacts but not in the records.ingest.start entry", ing, art)
			default:
				if seen[ing] == nil {
					seen[ing] = map[string]bool{}
				}
				seen[ing][art] = true
			}
		}
		if len(chunk) < verifyChunkRows {
			break
		}
		last := chunk[len(chunk)-1]
		afterIngest, afterArtifact, first = last[0], last[1], false
	}
	ids := make([]string, 0, len(expected))
	for id := range expected {
		if runs[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, art := range expected[id].start.Artifacts {
			if !seen[id][art] {
				ps.add("coverage", "run coverage of ingest %q: artifact %q is in the records.ingest.start entry but not in record_run_artifacts", id, art)
			}
		}
	}
}

// supersededDiff lists the pairs in a that are not in b (both SELECTs of
// ingest_id, artifact_id), at most verifyMaxPerKind+1 of them.
func supersededDiff(a, b string) string {
	return `SELECT ingest_id, artifact_id FROM (` + a + `) EXCEPT SELECT ingest_id, artifact_id FROM (` + b + `) ORDER BY 1, 2 LIMIT ` +
		fmt.Sprint(verifyMaxPerKind+1)
}

const storedSupersededSQL = `SELECT ingest_id, artifact_id FROM record_superseded`

// checkSupersession requires record_superseded to equal its recomputation from
// the runs (SupersededPairsSQL, the one definition shared with the writer and the
// reader), both ways: a deleted pair would show records a complete newer run
// supersedes, an injected one would hide records.
func (c *Case) checkSupersession(ctx context.Context, rep *VerifyReport, ps *problemSet) {
	type diff struct {
		query, kind, format string
	}
	for _, d := range []diff{
		{
			supersededDiff(storedSupersededSQL, SupersededPairsSQL), "superseded-extra",
			"record_superseded holds a pair no run implies: ingest %q, artifact %q (its records are hidden)",
		},
		{
			supersededDiff(SupersededPairsSQL, storedSupersededSQL), "superseded-missing",
			"record_superseded is missing the pair: ingest %q, artifact %q (its records are shown although a complete newer run supersedes them)",
		},
	} {
		var pairs [][2]string
		err := c.ReadTx(ctx, func(h ReadHandle) error {
			pairs = nil
			rows, err := h.QueryContext(ctx, d.query)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var p [2]string
				if err := rows.Scan(&p[0], &p[1]); err != nil {
					return err
				}
				pairs = append(pairs, p)
			}
			return rows.Err()
		})
		if err != nil {
			unreadable(rep, "record_superseded", err)
			return
		}
		for i, p := range pairs {
			if i == verifyMaxPerKind {
				rep.problemf("further record_superseded differences of this kind are not listed")
				break
			}
			ps.add(d.kind, d.format, p[0], p[1])
		}
	}
}
