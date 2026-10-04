package evidence

import (
	"context"
	"database/sql"
	"io"
	"reflect"
	"slices"
	"testing"
)

func auditAppend(t *testing.T, c *Case, action string, d interface{ Details() map[string]any }) AuditEntry {
	t.Helper()
	e, err := c.Audit.Append(action, "", d.Details())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// insertRunRow writes a parsers row (if absent) and a run row, straight into the
// tables: evidence-internal tests may (the single-writer rule exempts this package).
func insertRunRow(t *testing.T, c *Case, ingestID string, endSeq int64) {
	t.Helper()
	err := c.StoreTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO parsers (name, version) VALUES ('p', '1')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO record_runs (end_seq, ingest_id, parser_id, outcome, batches, records, first_id, last_id, rollup, ended)
			VALUES (?, ?, (SELECT id FROM parsers WHERE name = 'p' AND version = '1'), 'complete', 0, 0, 0, 0, 'r', 't')`, endSeq, ingestID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func insertBatchRow(t *testing.T, c *Case, ingestID string, no int) {
	t.Helper()
	err := c.StoreTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO record_batches (ingest_id, batch_no, first_id, count, digest, created) VALUES (?, ?, 1, 1, 'd', 't')`, ingestID, no)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnresolvedIngests(t *testing.T) {
	start := func(id string) IngestStart {
		return IngestStart{IngestID: id, Parser: "p", ParserVersion: "1", Artifacts: []string{"a1"}, BatchRows: 10}
	}
	batch := func(id string, no int) BatchCommit {
		return BatchCommit{IngestID: id, BatchNo: no, FirstID: int64(no), Count: 1, Digest: "d", Artifacts: map[string]string{"a1": "s"}}
	}
	conclusion := func(id, outcome string) IngestConclusion {
		return IngestConclusion{IngestID: id, Outcome: outcome, Batches: 1, Records: 1, FirstID: 1, LastID: 1, Rollup: "r"}
	}

	type want struct {
		kind    string
		absent  []int
		recover bool
	}
	cases := []struct {
		name  string
		build func(t *testing.T, c *Case)
		want  []want
	}{
		{"clean: concluded with a run row", func(t *testing.T, c *Case) {
			auditAppend(t, c, ActionIngestStart, start("i1"))
			auditAppend(t, c, ActionBatch, batch("i1", 1))
			insertBatchRow(t, c, "i1", 1)
			e := auditAppend(t, c, ActionIngestEnd, conclusion("i1", "complete"))
			insertRunRow(t, c, "i1", e.Seq)
		}, nil},
		{"no ingest at all", func(*testing.T, *Case) {}, nil},
		{"unfinished: no end, announced batch never written", func(t *testing.T, c *Case) {
			auditAppend(t, c, ActionIngestStart, start("i1"))
			auditAppend(t, c, ActionBatch, batch("i1", 1))
		}, []want{{kind: IngestUnfinished, absent: []int{1}}}},
		{"unfinished: batch written, no end", func(t *testing.T, c *Case) {
			auditAppend(t, c, ActionIngestStart, start("i1"))
			auditAppend(t, c, ActionBatch, batch("i1", 1))
			insertBatchRow(t, c, "i1", 1)
		}, []want{{kind: IngestUnfinished}}},
		{"run-missing: end audited, no run row", func(t *testing.T, c *Case) {
			auditAppend(t, c, ActionIngestStart, start("i1"))
			auditAppend(t, c, ActionBatch, batch("i1", 1))
			insertBatchRow(t, c, "i1", 1)
			auditAppend(t, c, ActionIngestEnd, conclusion("i1", "complete"))
		}, []want{{kind: IngestRunMissing}}},
		{"run-missing: error entry", func(t *testing.T, c *Case) {
			auditAppend(t, c, ActionIngestStart, start("i1"))
			cn := conclusion("i1", "incomplete")
			cn.Error = "boom"
			auditAppend(t, c, ActionIngestError, cn)
		}, []want{{kind: IngestRunMissing}}},
		{"absent batch explained by batch.error is not absent", func(t *testing.T, c *Case) {
			auditAppend(t, c, ActionIngestStart, start("i1"))
			auditAppend(t, c, ActionBatch, batch("i1", 1))
			auditAppend(t, c, ActionBatchError, BatchFailure{IngestID: "i1", BatchNo: 1, Error: "disk full"})
			auditAppend(t, c, ActionBatch, batch("i1", 2))
		}, []want{{kind: IngestUnfinished, absent: []int{2}}}},
		{"recovered: recover audited and run row written", func(t *testing.T, c *Case) {
			auditAppend(t, c, ActionIngestStart, start("i1"))
			auditAppend(t, c, ActionBatch, batch("i1", 1))
			r := auditAppend(t, c, ActionIngestRecover, IngestRecover{IngestConclusion: conclusion("i1", "interrupted"), ByIngestID: "i2", BatchNos: []int{1}})
			insertRunRow(t, c, "i1", r.Seq)
		}, nil},
		{"recover audited but run row not written", func(t *testing.T, c *Case) {
			auditAppend(t, c, ActionIngestStart, start("i1"))
			auditAppend(t, c, ActionBatch, batch("i1", 1))
			auditAppend(t, c, ActionIngestRecover, IngestRecover{IngestConclusion: conclusion("i1", "interrupted"), ByIngestID: "i2", BatchNos: []int{1}})
		}, []want{{kind: IngestUnfinished, recover: true}}},
		{"two ingests, only the second unresolved, start order kept", func(t *testing.T, c *Case) {
			auditAppend(t, c, ActionIngestStart, start("i1"))
			e := auditAppend(t, c, ActionIngestEnd, conclusion("i1", "complete"))
			insertRunRow(t, c, "i1", e.Seq)
			auditAppend(t, c, ActionIngestStart, start("i2"))
			auditAppend(t, c, ActionIngestStart, start("i3"))
		}, []want{{kind: IngestUnfinished}, {kind: IngestUnfinished}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCase(t)
			tc.build(t, c)
			got, err := c.UnresolvedIngests()
			if err != nil {
				t.Fatal(err)
			}
			var gotW []want
			for _, u := range got {
				gotW = append(gotW, want{kind: u.Kind, absent: u.AbsentBatches, recover: u.Recover != nil})
			}
			if len(gotW) != len(tc.want) {
				t.Fatalf("unresolved = %+v, want %+v", gotW, tc.want)
			}
			for i := range gotW {
				if gotW[i].kind != tc.want[i].kind || gotW[i].recover != tc.want[i].recover || !slices.Equal(gotW[i].absent, tc.want[i].absent) {
					t.Errorf("unresolved[%d] = %+v, want %+v", i, gotW[i], tc.want[i])
				}
			}
			if len(got) == 2 && (got[0].Start.IngestID != "i2" || got[1].Start.IngestID != "i3") {
				t.Errorf("order = %s, %s", got[0].Start.IngestID, got[1].Start.IngestID)
			}
			for _, u := range got {
				if u.Kind == IngestRunMissing && (u.Conclusion == nil || u.ConclusionSeq == 0) {
					t.Errorf("run-missing without its conclusion: %+v", u)
				}
				if u.Kind == IngestUnfinished && u.Conclusion != nil {
					t.Errorf("unfinished with a conclusion: %+v", u)
				}
			}
		})
	}
}

// TestSupersededPairsSQLIsOrderInsensitive: the pairs depend on end_seq and
// outcome, never on the order the run rows were inserted in; only a complete
// run of the same parser name supersedes.
func TestSupersededPairsSQLIsOrderInsensitive(t *testing.T) {
	type run struct {
		ingest, parser, version, outcome string
		endSeq                           int64
		artifacts                        []string
	}
	runs := []run{
		{"old", "p", "1", "complete", 10, []string{"A", "B"}},
		{"new", "p", "2", "complete", 20, []string{"A"}},
		{"cut", "p", "3", "incomplete", 30, []string{"A", "B"}},
		{"other", "q", "1", "complete", 40, []string{"A", "B"}},
	}
	want := map[[2]string]bool{{"old", "A"}: true}
	orders := [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}}
	for _, order := range orders {
		c := newTestCase(t)
		ids := map[string]string{}
		for _, name := range []string{"A", "B"} {
			rec, err := c.Capture("dev1", "acq1", name, testSrc, func(w io.Writer) error { _, err := w.Write([]byte(name)); return err })
			if err != nil {
				t.Fatal(err)
			}
			ids[name] = rec.ID
		}
		err := c.StoreTx(context.Background(), func(tx *sql.Tx) error {
			for _, i := range order {
				r := runs[i]
				if _, err := tx.Exec(`INSERT OR IGNORE INTO parsers (name, version) VALUES (?, ?)`, r.parser, r.version); err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO record_runs (end_seq, ingest_id, parser_id, outcome, batches, records, first_id, last_id, rollup, ended)
					VALUES (?, ?, (SELECT id FROM parsers WHERE name = ? AND version = ?), ?, 0, 0, 0, 0, 'r', 't')`,
					r.endSeq, r.ingest, r.parser, r.version, r.outcome); err != nil {
					return err
				}
				for _, a := range r.artifacts {
					if _, err := tx.Exec(`INSERT INTO record_run_artifacts (ingest_id, artifact_id) VALUES (?, ?)`, r.ingest, ids[a]); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got := map[[2]string]bool{}
		name := map[string]string{ids["A"]: "A", ids["B"]: "B"}
		err = c.ReadTx(context.Background(), func(h ReadHandle) error {
			rows, err := h.Query(SupersededPairsSQL)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var ing, art string
				if err := rows.Scan(&ing, &art); err != nil {
					return err
				}
				got[[2]string{ing, name[art]}] = true
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("insert order %v: pairs = %v, want %v", order, got, want)
		}
	}
}
