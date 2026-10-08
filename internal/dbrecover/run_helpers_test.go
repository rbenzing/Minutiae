package dbrecover

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

type runEnv struct {
	c                *evidence.Case
	db, wal, journal evidence.ManifestRecord
	f                dbFiles
}

func newRunEnv(t *testing.T) *runEnv {
	t.Helper()
	c := recordstest.NewCase(t)
	f := newBuilderDB(t)
	return &runEnv{
		c: c, f: f,
		db:      addFile(t, c, "/data/x.db", f.db),
		wal:     addFile(t, c, "/data/x.db-wal", f.wal),
		journal: addFile(t, c, "/data/x.db-journal", f.journal),
	}
}

func (e *runEnv) in() Inputs { return Inputs{DB: e.db.ID} }

func mustRun(t *testing.T, c *evidence.Case, in Inputs, opts Options) Result {
	t.Helper()
	res, err := Run(context.Background(), c, in, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func readAudit(t testing.TB, c *evidence.Case) []evidence.AuditEntry {
	t.Helper()
	es, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func firstIndex(es []evidence.AuditEntry, action string) int {
	for i, e := range es {
		if e.Action == action {
			return i
		}
	}
	return -1
}

func indexes(es []evidence.AuditEntry, action string) []int {
	var out []int
	for i, e := range es {
		if e.Action == action {
			out = append(out, i)
		}
	}
	return out
}

func count(es []evidence.AuditEntry, action string) int { return len(indexes(es, action)) }

func lastEntry(t testing.TB, es []evidence.AuditEntry, action string) evidence.AuditEntry {
	t.Helper()
	ix := indexes(es, action)
	if len(ix) == 0 {
		t.Fatalf("no %s entry in the audit log", action)
	}
	return es[ix[len(ix)-1]]
}

func storedRecords(t testing.TB, c *evidence.Case) []records.Full {
	t.Helper()
	r, err := records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	var out []records.Full
	cursor := ""
	for {
		res, err := r.List(context.Background(), records.Filter{IncludeSuperseded: true}, records.Page{Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range res.Rows {
			f, err := r.Get(context.Background(), row.ID)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, f)
		}
		if res.NextCursor == "" {
			return out
		}
		cursor = res.NextCursor
	}
}

func payloadOf(t testing.TB, f records.Full) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(f.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func sumMap(m map[string]int64) (n int64) {
	for _, v := range m {
		n += v
	}
	return n
}

func libraryRowsByMethod(t *testing.T, f dbFiles) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, r := range f.historyWith(t, f.journal) {
		out[r.Method]++
	}
	return out
}

func libraryWarnings(t *testing.T, f dbFiles) []sqlitefile.Warning {
	t.Helper()
	db, err := sqlitefile.Open(bytesAt{f.db}, int64(len(f.db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AttachWAL(bytesAt{f.wal}, int64(len(f.wal))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AttachJournal(bytesAt{f.journal}, int64(len(f.journal))); err != nil {
		t.Fatal(err)
	}
	h := db.History()
	defer h.Release()
	if _, err := h.Summary(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Rows(context.Background(), func(sqlitefile.RecoveredRow) bool { return true }); err != nil {
		t.Fatal(err)
	}
	return append(db.Warnings(), h.Warnings()...)
}

// sqlitetestWAL4k is a valid WAL of a 4096-byte-page database: attached to the
// 1024-byte-page scenario database it raises wal-page-size-mismatch.
func sqlitetestWAL4k(t testing.TB) []byte {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	tb := b.CreateTable("t", "create table t(a, b)")
	tb.Insert(1, 1, filler(50, 1))
	pg := b.Bytes()[4096:8192]
	w := b.NewWAL(false, 0x2000, 0x2001, 0)
	w.Frame(2, pg, 2)
	return w.Bytes()
}

// gunzipFile reads a committed fixture; ok is false when it does not exist.
func gunzipFile(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b, true
}

func fixtureNames(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "sqlitefile", "testdata", "*.db.gz"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	var names []string
	for _, p := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(p), ".db.gz"))
	}
	return names
}

func fixtureCase(t *testing.T, name string) (*evidence.Case, Inputs) {
	t.Helper()
	dir := filepath.Join("..", "sqlitefile", "testdata")
	c := recordstest.NewCase(t)
	db, _ := gunzipFile(t, filepath.Join(dir, name+".db.gz"))
	rec := addFile(t, c, "/data/"+name+".db", db)
	if w, ok := gunzipFile(t, filepath.Join(dir, name+".db-wal.gz")); ok {
		addFile(t, c, "/data/"+name+".db-wal", w)
	}
	if j, ok := gunzipFile(t, filepath.Join(dir, name+".db-journal.gz")); ok {
		addFile(t, c, "/data/"+name+".db-journal", j)
	}
	return c, Inputs{DB: rec.ID}
}

func refusalReason(err error) string {
	var re *RefusalError
	if errors.As(err, &re) {
		return re.Reason
	}
	if err != nil {
		return "other: " + err.Error()
	}
	return ""
}

func nonEmpty(m map[string]int64) map[string]int64 {
	if len(m) == 0 {
		return map[string]int64{}
	}
	return m
}
