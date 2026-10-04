package records_test

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// baseCase is a closed case on disk: two artifacts, two parsers; ingest 1 holds
// the two full records (ids 1 and 2, one batch), ingest 2 one more record of the
// second parser (id 3).
type baseCase struct {
	dir     string
	art     evidence.ManifestRecord
	art2    evidence.ManifestRecord
	ing1    string
	parser2 int64
}

func buildBaseCase(t *testing.T) baseCase {
	t.Helper()
	c, a := setup(t)
	a2 := recordstest.AddArtifact(t, c, "b.db", append([]byte("second"), mib...))
	res1 := recordstest.Ingest(t, c, testParser, []string{a.ID}, []records.Record{
		fullRecord(a.ID, 1, records.BasisLocalOffset, 60, true),
		fullRecord(a.ID, 2, records.BasisLocalUnknown, 0, false),
	})
	other := records.Parser{Name: "other-parser", Version: "2.0", Hash: "def456"}
	recordstest.Ingest(t, c, other, []string{a2.ID}, recordstest.Records(a2.ID, 1, 3))
	p2 := scalar[int64](t, c, `SELECT id FROM parsers WHERE name = 'other-parser'`)
	if rep := mustVerify(t, c); !rep.OK() {
		t.Fatalf("the base case is not clean: %q", rep.Problems)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return baseCase{dir: c.Dir, art: a, art2: a2, ing1: res1.IngestID, parser2: p2}
}

// cloneCase copies a closed case directory (not its lock) and returns the copy's
// directory, so a subtest tampers with its own copy of one shared base case.
func cloneCase(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		if d.Name() == "case.lock" {
			return nil
		}
		in, err := os.Open(p) //nolint:gosec // copying a case directory the test created
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(filepath.Join(dst, rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // inside the test's own temp dir
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func openClone(t *testing.T, dir string) *evidence.Case {
	t.Helper()
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestVerifyDetectsAnyColumnAlteration: every column of every record table, changed
// alone (triggers restored), is caught with its specific problem. The case is
// built once and copied for each subtest.
//
// Record 1: local-offset time, deleted and recovered, times created (local-offset)
// and modified (local-unknown). Record 2: local-unknown time, deleted, not
// recovered. Record 3 belongs to ingest 2.
func TestVerifyDetectsAnyColumnAlteration(t *testing.T) {
	base := buildBaseCase(t)
	type alter struct {
		name    string
		change  func(dir string)
		want    []string
		allowed []string
		batch1  bool // every problem must name ingest 1
	}
	setCol := func(id int64, col string, v any) func(string) {
		return func(dir string) { recordstest.SetRecordColumn(t, dir, id, col, v) }
	}
	digestOnly := func(name string, change func(dir string)) alter {
		return alter{name: name, change: change, want: []string{"digest mismatch"}, batch1: true}
	}
	// a changed summary or body, or a changed id, also leaves the full-text index (built from the
	// original rows) differing from a rebuild: P11 reports that too
	digestAndIndex := func(name string, change func(dir string)) alter {
		a := digestOnly(name, change)
		a.allowed = []string{"records_fts"}
		return a
	}
	batchCol := func(name, col string, v any, want ...string) alter {
		return alter{name: "record_batches." + name, want: want, change: func(dir string) {
			recordstest.SetBatchColumn(t, dir, base.ing1, 1, col, v)
		}}
	}
	parserCol := func(col string, v any, want ...string) alter {
		return alter{name: "parsers." + col, want: want, change: func(dir string) { recordstest.SetParserColumn(t, dir, 1, col, v) }}
	}
	timeCol := func(id int64, kind, col string, v any) func(string) {
		return func(dir string) { recordstest.SetRecordTimeColumn(t, dir, id, kind, col, v) }
	}
	parserNamed := []string{"digest mismatch", "not named by any records.ingest.start", "run row of ingest"}
	notAnnounced := []string{"batch row missing", "is not announced by any records.batch audit entry"}
	cases := []alter{
		// the records table, column by column
		digestOnly("records.type", setCol(1, "type", "call")),
		digestOnly("records.payload_v", setCol(1, "payload_v", 2)),
		digestOnly("records.source_path", setCol(1, "source_path", "/other.db")),
		digestOnly("records.source_path to NULL", setCol(1, "source_path", nil)),
		digestOnly("records.locator", setCol(1, "locator", "sqlite:t=other")),
		digestOnly("records.src_offset", setCol(1, "src_offset", 101)),
		digestOnly("records.src_length", setCol(1, "src_length", 51)),
		digestOnly("records.ts", setCol(1, "ts", 1_650_000_001)),
		digestOnly("records.ts_end", setCol(1, "ts_end", 1_650_000_099)),
		digestOnly("records.ts_basis", setCol(2, "ts_basis", "utc")),
		digestOnly("records.tz_offset_min", setCol(1, "tz_offset_min", 90)),
		digestOnly("records.deleted", setCol(2, "deleted", 0)),
		digestOnly("records.recovered", func(dir string) {
			recordstest.SetRecordColumns(t, dir, 2, map[string]any{"recovered": 1, "recovery_method": "carve"})
		}),
		digestOnly("records.recovery_method", setCol(1, "recovery_method", "other")),
		digestOnly("records.confidence", setCol(1, "confidence", 81)),
		digestAndIndex("records.summary", setCol(1, "summary", "changed")),
		digestAndIndex("records.body", setCol(1, "body", "changed body")),
		digestOnly("records.payload", setCol(1, "payload", `{"a":2}`)),
		digestOnly("records.artifact_id", func(dir string) { recordstest.RepointRecord(t, dir, 1, base.art2.ID) }),
		digestOnly("records.parser_id", func(dir string) { recordstest.SetRecordParser(t, dir, 1, base.parser2) }),
		{
			name: "records.batch_id", change: setCol(1, "batch_id", 999),
			want: []string{"batch id 999 does not exist", "is not the batch that announced"},
		},
		{
			name: "records.id to 0", change: func(dir string) { recordstest.SetRecordID(t, dir, 1, 0) },
			want:    []string{"id is not positive", "outside every batch range", "records stored", "digest mismatch", "has no record"},
			allowed: []string{"records_fts"},
		},
		{
			name: "records.id to an unused id", change: func(dir string) { recordstest.SetRecordID(t, dir, 1, 7) },
			want:    []string{"outside every batch range", "records stored", "digest mismatch", "has no record", "next_id"},
			allowed: []string{"records_fts"},
		},
		// record_times, column by column
		digestOnly("record_times.kind", timeCol(1, "created", "kind", "other")),
		digestOnly("record_times.ts", func(dir string) { recordstest.SetRecordTime(t, dir, 1, "created", 5) }),
		digestOnly("record_times.ts_basis", timeCol(1, "modified", "ts_basis", "utc")),
		digestOnly("record_times.tz_offset_min", timeCol(1, "created", "tz_offset_min", 90)),
		{name: "record_times.record_id", change: timeCol(1, "created", "record_id", 99), want: []string{"has no record", "digest mismatch"}},
		digestOnly("record_times row deleted", func(dir string) { recordstest.DeleteRecordTime(t, dir, 1, "modified") }),
		digestOnly("record_times row injected", func(dir string) { recordstest.InjectRecordTime(t, dir, 1, "accessed", 77) }),
		// parsers, column by column
		parserCol("name", "evil", parserNamed...),
		parserCol("version", "9.9", parserNamed...),
		parserCol("hash", "ffff", parserNamed...),
		parserCol("id", 55, "parser id 1 does not exist", "digest mismatch"),
		// record_batches, column by column
		batchCol("first_id", "first_id", 20, "differs from the audit log"),
		batchCol("count", "count", 3, "differs from the audit log"),
		batchCol("digest", "digest", strings.Repeat("0", 64), "differs from the audit log"),
		batchCol("created", "created", "2001-01-01T00:00:00Z", "differs from the audit log"),
		batchCol("ingest_id", "ingest_id", "ing-other", notAnnounced...),
		batchCol("batch_no", "batch_no", 5, notAnnounced...),
		batchCol("batch_id", "batch_id", 77, "batch id 1 does not exist", "is not the batch that announced"),
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := cloneCase(t, base.dir)
			tc.change(dir)
			rep := mustVerify(t, openClone(t, dir))
			expectProblems(t, rep, tc.want, tc.allowed...)
			if tc.batch1 {
				for _, p := range rep.Problems {
					if !strings.Contains(p, "ingest "+`"`+base.ing1+`"`) && !strings.Contains(p, "records_fts") {
						t.Errorf("the problem does not name the ingest of the record: %s", p)
					}
				}
			}
		})
	}
}
