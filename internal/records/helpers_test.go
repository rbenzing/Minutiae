package records_test

import (
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

var (
	ctx        = context.Background()
	testParser = records.Parser{Name: "sms-parser", Version: "1.0", Hash: "abc123"}
	mib        = make([]byte, 1<<20)
)

// setup returns a case with one 1 MiB artifact.
func setup(t *testing.T) (*evidence.Case, evidence.ManifestRecord) {
	t.Helper()
	c := recordstest.NewCase(t)
	return c, recordstest.AddArtifact(t, c, "a.db", mib)
}

func newWriter(t *testing.T, c *evidence.Case, p records.Parser, o records.WriterOptions) *records.Writer {
	t.Helper()
	w, err := records.NewWriter(c, p, o)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func startWriter(t *testing.T, c *evidence.Case, p records.Parser, o records.WriterOptions, artifacts ...string) *records.Writer {
	t.Helper()
	w := newWriter(t, c, p, o)
	if err := w.Start(ctx, records.StartOptions{Artifacts: artifacts}); err != nil {
		t.Fatal(err)
	}
	return w
}

func add(t *testing.T, w *records.Writer, recs []records.Record) {
	t.Helper()
	for _, r := range recs {
		if err := w.Add(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}

// scalar runs a read query that returns one value.
func scalar[T any](t *testing.T, c *evidence.Case, q string, args ...any) T {
	t.Helper()
	var v T
	err := c.ReadTx(ctx, func(h evidence.ReadHandle) error { return h.QueryRow(q, args...).Scan(&v) })
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func count(t *testing.T, c *evidence.Case, table string) int {
	t.Helper()
	return scalar[int](t, c, `SELECT count(*) FROM `+table)
}

func auditOf(t *testing.T, c *evidence.Case, action string) []evidence.AuditEntry {
	t.Helper()
	all, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.AuditEntry
	for _, e := range all {
		if action == "" || e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func details[T any](t *testing.T, e evidence.AuditEntry) T {
	t.Helper()
	d, err := evidence.DecodeDetails[T](e.Details)
	if err != nil {
		t.Fatalf("audit seq %d (%s): %v", e.Seq, e.Action, err)
	}
	return d
}

// loadRows reads every stored record as the digest sees it, ordered by id.
func loadRows(t *testing.T, c *evidence.Case) []evidence.RecordRow {
	t.Helper()
	var out []evidence.RecordRow
	err := c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		rows, err := h.Query(`SELECT r.id, r.type, r.payload_v, r.artifact_id, r.source_path, r.locator, r.src_offset, r.src_length,
			r.ts, r.ts_end, r.ts_basis, r.tz_offset_min, r.deleted, r.recovered, r.recovery_method, r.confidence,
			p.name, p.version, p.hash, r.summary, r.body, r.payload
			FROM records r JOIN parsers p ON p.id = r.parser_id ORDER BY r.id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r evidence.RecordRow
			if err := rows.Scan(&r.ID, &r.Type, &r.PayloadV, &r.ArtifactID, &r.SourcePath, &r.Locator, &r.SrcOffset, &r.SrcLength,
				&r.TS, &r.TSEnd, &r.TSBasis, &r.TZOffsetMin, &r.Deleted, &r.Recovered, &r.RecoveryMethod, &r.Confidence,
				&r.ParserName, &r.ParserVersion, &r.ParserHash, &r.Summary, &r.Body, &r.Payload); err != nil {
				return err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		trows, err := h.Query(`SELECT record_id, kind, ts, ts_basis, tz_offset_min FROM record_times ORDER BY record_id, kind`)
		if err != nil {
			return err
		}
		defer func() { _ = trows.Close() }()
		byID := map[int64]int{}
		for i, r := range out {
			byID[r.ID] = i
		}
		for trows.Next() {
			var id int64
			var tm evidence.RecordTime
			if err := trows.Scan(&id, &tm.Kind, &tm.TS, &tm.Basis, &tm.TZOffsetMin); err != nil {
				return err
			}
			out[byID[id]].Times = append(out[byID[id]].Times, tm)
		}
		return trows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type pair struct{ Ingest, Artifact string }

func superseded(t *testing.T, c *evidence.Case) map[pair]bool {
	t.Helper()
	out := map[pair]bool{}
	err := c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		rows, err := h.Query(`SELECT ingest_id, artifact_id FROM record_superseded`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.Ingest, &p.Artifact); err != nil {
				return err
			}
			out[p] = true
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// crash is the sentinel a test hook panics with to simulate a dead process.
type crash struct{ point string }

// crashAt makes the writer panic with a crash at the nth call of point.
func crashAt(w *records.Writer, point string, nth int) {
	calls := 0
	w.SetHook(func(p string) error {
		if p == point {
			calls++
			if calls == nth {
				w.Die() // the "process" is gone: it holds no live-ingest slot
				panic(crash{p})
			}
		}
		return nil
	})
}

// survive runs fn and reports whether it ended in a simulated crash.
func survive(t *testing.T, fn func()) (crashed bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(crash); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	fn()
	return false
}
