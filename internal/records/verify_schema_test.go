package records_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// Schema-object tampering (final review C2): redefining an index, adding a view,
// changing a table definition or dropping an index changes what every reader query
// returns without touching a row, so no row digest sees it; `quick_check` does not
// check indexes against their tables either.

func replaceOnce(t *testing.T, s, old, repl string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("%q does not contain %q", s, old)
	}
	return strings.Replace(s, old, repl, 1)
}

// schemaCases are the schema tampering cases: each changes the database of the
// given directory and names the problem verify must report.
func schemaCases(t *testing.T) []struct {
	name   string
	change func(dir string)
	want   string
} {
	return []struct {
		name   string
		change func(dir string)
		want   string
	}{
		{"index redefined", func(dir string) {
			def := recordstest.SchemaSQL(t, dir, "index", "records_deleted")
			recordstest.RedefineSchemaObject(t, dir, "index", "records_deleted", replaceOnce(t, def, "deleted = 1", "deleted = 0"))
		}, `index "records_deleted" was altered`},
		{"index dropped", func(dir string) {
			recordstest.DropSchemaObject(t, dir, "index", "records_ts")
		}, `index "records_ts" is missing from the schema`},
		{"table definition altered (CHECK removed)", func(dir string) {
			def := recordstest.SchemaSQL(t, dir, "table", "records")
			recordstest.RedefineSchemaObject(t, dir, "table", "records", replaceOnce(t, def, "CHECK (confidence IS NULL OR confidence BETWEEN 0 AND 100),", ""))
		}, `table "records" was altered`},
		{"table definition altered (collation)", func(dir string) {
			def := recordstest.SchemaSQL(t, dir, "table", "records")
			recordstest.RedefineSchemaObject(t, dir, "table", "records", replaceOnce(t, def, "type TEXT NOT NULL", "type TEXT COLLATE NOCASE NOT NULL"))
		}, `table "records" was altered`},
		{"table definition altered (STRICT removed)", func(dir string) {
			def := recordstest.SchemaSQL(t, dir, "table", "record_batches")
			recordstest.RedefineSchemaObject(t, dir, "table", "record_batches", replaceOnce(t, def, ") STRICT", ")"))
		}, `table "record_batches" was altered`},
		{"view added", func(dir string) {
			recordstest.CreateSchemaObject(t, dir, `CREATE VIEW records_visible AS SELECT * FROM records WHERE deleted = 0`)
		}, `view "records_visible" is not part of the schema`},
		{"table added", func(dir string) {
			recordstest.CreateSchemaObject(t, dir, `CREATE TABLE shadow (id INTEGER PRIMARY KEY)`)
		}, `table "shadow" is not part of the schema`},
		{"index added", func(dir string) {
			recordstest.CreateSchemaObject(t, dir, `CREATE INDEX records_extra ON records (summary)`)
		}, `index "records_extra" is not part of the schema`},
		{"trigger added", func(dir string) {
			recordstest.CreateSchemaObject(t, dir, `CREATE TRIGGER records_sneaky AFTER INSERT ON records_meta BEGIN SELECT 1; END`)
		}, `trigger "records_sneaky" is not part of the schema`},
	}
}

func TestVerifyFlagsSchemaObjectTampering(t *testing.T) {
	base := buildBaseCase(t)
	for _, tc := range schemaCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			dir := cloneCase(t, base.dir)
			tc.change(dir)
			rep := mustVerify(t, openClone(t, dir))
			// integrity_check may also complain (an index that no longer matches its table)
			expectProblems(t, rep, []string{tc.want}, "integrity_check failed")
		})
	}
}

// TestVerifyFlagsIndexDesyncWithIntactSchemaText: the stored definitions are all
// as expected, but an index b-tree disagrees with its table (page-level
// tampering). quick_check says "ok"; only integrity_check sees it.
func TestVerifyFlagsIndexDesyncWithIntactSchemaText(t *testing.T) {
	base := buildBaseCase(t)
	dir := cloneCase(t, base.dir)
	recordstest.DesyncIndex(t, dir, "records_ts", `CREATE INDEX records_ts ON records (id)`)
	c := openClone(t, dir)

	var quick string
	if err := c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		return h.QueryRow(`SELECT quick_check FROM pragma_quick_check`).Scan(&quick)
	}); err != nil || quick != "ok" {
		t.Fatalf("the scenario needs quick_check to say ok: %q, %v", quick, err)
	}
	rep := mustVerify(t, c)
	expectProblems(t, rep, []string{"integrity_check failed"})
	for _, p := range rep.Problems {
		if strings.Contains(p, "schema") || strings.Contains(p, "was altered") {
			t.Errorf("the schema text is intact, yet: %s", p)
		}
	}
}

// TestVerifyFlagsPlantedObjectsInV1Case: a v1 case is compared with the v1 schema
// too, so v2-shaped tables planted in it are reported.
func TestVerifyFlagsPlantedObjectsInV1Case(t *testing.T) {
	dir := recordstest.NewV1Case(t)
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if rep := mustVerify(t, c); !rep.OK() {
		t.Fatalf("a clean v1 case must verify: %q", rep.Problems)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	recordstest.CreateSchemaObject(t, dir, `CREATE TABLE record_batches (batch_id INTEGER PRIMARY KEY)`)
	c2, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	rep := mustVerify(t, c2)
	expectProblems(t, rep, []string{`table "record_batches" is not part of the schema`})
}

// TestReaderRefusesTamperedSchema: the reader runs only on a database whose
// schema objects match; every method returns an integrity error (exit 4), never
// an answer shaped by a redefined index or view.
func TestReaderRefusesTamperedSchema(t *testing.T) {
	base := buildBaseCase(t)
	for _, tc := range schemaCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			dir := cloneCase(t, base.dir)
			c := openClone(t, dir)
			r, err := records.NewReader(c)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.List(ctx, records.Filter{}, records.Page{}); err != nil {
				t.Fatalf("control: the untampered reader failed: %v", err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			tc.change(dir)
			c = openClone(t, dir)
			r, err = records.NewReader(c)
			if err != nil {
				t.Fatal(err)
			}
			for name, call := range map[string]func() error{
				"List":     func() error { _, err := r.List(ctx, records.Filter{}, records.Page{}); return err },
				"Count":    func() error { _, _, err := r.Count(ctx, records.Filter{}, 100); return err },
				"Get":      func() error { _, err := r.Get(ctx, 1); return err },
				"Stats":    func() error { _, err := r.Stats(ctx, records.Filter{}, "type"); return err },
				"Overview": func() error { _, err := r.Overview(ctx, records.Filter{}); return err },
			} {
				err := call()
				if !errors.Is(err, evidence.ErrIntegrity) {
					t.Errorf("%s on a tampered schema: %v, want an integrity error", name, err)
				} else if !strings.Contains(err.Error(), "case verify") {
					t.Errorf("%s: the error does not point at case verify: %v", name, err)
				}
			}
		})
	}
}

// TestWriterRefusesTamperedSchema: Start audits and writes nothing against a
// database whose schema objects are not the expected ones.
func TestWriterRefusesTamperedSchema(t *testing.T) {
	base := buildBaseCase(t)
	dir := cloneCase(t, base.dir)
	recordstest.CreateSchemaObject(t, dir, `CREATE VIEW records_visible AS SELECT * FROM records`)
	c := openClone(t, dir)
	before := len(auditOf(t, c, ""))
	w := newWriter(t, c, records.Parser{Name: "p", Version: "1"}, records.WriterOptions{})
	err := w.Start(ctx, records.StartOptions{Artifacts: []string{base.art.ID}})
	if !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("Start on a tampered schema: %v", err)
	}
	if after := len(auditOf(t, c, "")); after != before {
		t.Errorf("Start audited %d entries before refusing", after-before)
	}
}
