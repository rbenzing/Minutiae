package dbrecover

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const testPageSize = 1024

// dbFiles is a database with a WAL and a hot journal written by the
// sqlitetest builder.
type dbFiles struct {
	db, wal, journal, persist []byte // persist: a journal whose header was zeroed
}

func filler(n int, seed byte) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte('a' + byte((i*7+int(seed))%26))
	}
	return b.String()
}

func buildTable(rev byte) *sqlitetest.Builder {
	b := sqlitetest.New(sqlitetest.Options{PageSize: testPageSize})
	tb := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 12; id++ {
		tb.Insert(id, id*3, filler(100, byte(id)+rev))
	}
	return b
}

// newBuilderDB builds, at page size 1024: a database whose pages 2..4 hold
// table t (page 4 changed after a journal was written), a freelist (trunk 5,
// leaf 6) whose leaf keeps a stale leaf of t, a WAL with two committed frames
// of page 3 (the older is superseded) and a hot journal holding the earlier
// image of page 4.
func newBuilderDB(t testing.TB) dbFiles {
	t.Helper()
	b1, b2 := buildTable(0), buildTable(40)
	b1.SetFreelist([][]uint32{{5, 6}})
	db := b1.Bytes()
	if len(db) != 6*testPageSize {
		t.Fatalf("scenario database has %d pages, want 6", len(db)/testPageSize)
	}
	old3 := append([]byte(nil), db[2*testPageSize:3*testPageSize]...)
	old4 := append([]byte(nil), db[3*testPageSize:4*testPageSize]...)
	copy(db[5*testPageSize:], old3) // stale leaf in the freelist leaf
	n2 := b2.Bytes()
	new3 := n2[2*testPageSize : 3*testPageSize]
	new4 := n2[3*testPageSize : 4*testPageSize]
	copy(db[3*testPageSize:], new4)
	w := b1.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(3, old3, 6)
	w.Frame(3, new3, 6)
	j := b1.NewJournal(512, 0xdeadbeef, 6)
	j.Record(4, old4)
	jp := b1.NewJournal(512, 0xfeedface, 6)
	jp.Record(4, old4)
	jp.Zero()
	return dbFiles{db: db, wal: w.Bytes(), journal: j.Bytes(), persist: jp.Bytes()}
}

type bytesAt struct{ b []byte }

func (r bytesAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// history returns every recovered row of the scenario.

func (f dbFiles) historyWith(t testing.TB, journal []byte) []sqlitefile.RecoveredRow {
	t.Helper()
	db, err := sqlitefile.Open(bytesAt{f.db}, int64(len(f.db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AttachWAL(bytesAt{f.wal}, int64(len(f.wal))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AttachJournal(bytesAt{journal}, int64(len(journal))); err != nil {
		t.Fatal(err)
	}
	h := db.History()
	defer h.Release()
	var rows []sqlitefile.RecoveredRow
	if _, err := h.Rows(context.Background(), func(r sqlitefile.RecoveredRow) bool {
		rows = append(rows, r)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// addFile stores data as a "file" artifact of acquisition acq1.
func addFile(t testing.TB, c *evidence.Case, remotePath string, data []byte) evidence.ManifestRecord {
	t.Helper()
	src := evidence.Source{Kind: "file", DeviceID: "dev1", RemotePath: remotePath}
	rec, err := c.Capture("dev1", "acq1", strings.TrimPrefix(remotePath, "/"), src, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// addDerivedFile stores data as an extract-kind artifact derived from parent.
func addDerivedFile(t testing.TB, c *evidence.Case, parent evidence.ManifestRecord, name string, data []byte) evidence.ManifestRecord {
	t.Helper()
	return recordstest.AddDerived(t, c, parent, name, "extract", data, nil)
}

// requireLimitWarning fails unless an analysis.warning names the limit.
func requireLimitWarning(t testing.TB, es []evidence.AuditEntry, limit string) {
	t.Helper()
	for _, i := range indexes(es, "analysis.warning") {
		if r, _ := es[i].Details["reason"].(string); strings.Contains(r, "limit "+limit+" reached") {
			return
		}
	}
	t.Errorf("no analysis.warning names the limit %s", limit)
}
