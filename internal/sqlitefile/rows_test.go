package sqlitefile_test

// History rows (plan 3I, Task 12): structural cells of the history's page
// images, table identity and the relation of each row to the live rows.

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// rowScen is a database of two tables: t(a, b) on page 2 (rows 1..3) and
// u(x, y, z) on page 3 (rows 1..2). Row versions differ in the numbers and in
// the text but never in length, so an update rewrites the cell in place.
type rowScen struct {
	b     *sqlitetest.Builder
	t     *sqlitetest.Table
	u     *sqlitetest.Table
	db0   []byte // the database as built (rollback mode), version 0
	v0    *sqlitetest.Image
	cellT map[int64][]byte // the cells of t at version 0
}

func tText(id, v int64) string { return fmt.Sprintf("row%d-v%d-padpadpad", id, v) }

func newRowScen(t testing.TB) *rowScen {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	s := &rowScen{b: b, cellT: map[int64][]byte{}}
	s.t = b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 3; id++ {
		s.t.Insert(id, id*10, tText(id, 0))
	}
	s.u = b.CreateTable("u", "create table u(x, y, z)")
	for id := int64(1); id <= 2; id++ {
		s.u.Insert(id, id, "u-text", id*100)
	}
	if got := s.t.Leaves(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("t leaves %v", got)
	}
	if got := s.u.Leaves(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("u leaves %v", got)
	}
	for id := int64(1); id <= 3; id++ {
		s.cellT[id] = s.cellOf(id)
	}
	s.v0 = b.Snapshot()
	s.db0 = withCount(b.Bytes(), s.v0.Pages())
	return s
}

// setT rewrites row id of t to version v and returns the page image and the
// cell as laid out.
func (s *rowScen) setT(id, v int64) (*sqlitetest.Image, []byte, int) {
	s.t.Update(id, id*10+v, tText(id, v))
	cell, _, off := s.t.CellBytes(id)
	return s.b.Snapshot(), cell, off
}

func (s *rowScen) setU(id, v int64) *sqlitetest.Image {
	s.u.Update(id, id+v, "u-text", id*100+v)
	return s.b.Snapshot()
}

func (s *rowScen) cellOf(id int64) []byte {
	c, _, _ := s.t.CellBytes(id)
	return c
}

func openAll(t testing.TB, db, wal, journal []byte) (*sqlitefile.DB, *sqlitefile.Hist) {
	t.Helper()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if wal != nil {
		if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
			t.Fatal(err)
		}
	}
	if journal != nil {
		if _, err := d.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
			t.Fatal(err)
		}
	}
	h := d.History()
	t.Cleanup(h.Release)
	return d, h
}

func collectRows(t testing.TB, h *sqlitefile.Hist) ([]sqlitefile.RecoveredRow, sqlitefile.RowStats) {
	t.Helper()
	var rows []sqlitefile.RecoveredRow
	st, err := h.Rows(context.Background(), func(r sqlitefile.RecoveredRow) bool {
		rows = append(rows, r)
		return true
	})
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	return rows, st
}

// files holds the bytes of the three files of a scenario.
type files struct{ db, wal, journal []byte }

func (f files) of(k sqlitefile.FileKind) []byte {
	switch k {
	case sqlitefile.FileDB:
		return f.db
	case sqlitefile.FileWAL:
		return f.wal
	case sqlitefile.FileJournal:
		return f.journal
	}
	return nil
}

// assertCell checks that the bytes of the row's Loc are the cell the builder
// placed there.
func assertCell(t testing.TB, f files, r sqlitefile.RecoveredRow, want []byte) {
	t.Helper()
	b := f.of(r.Loc.File)
	end := r.Loc.Offset + r.Loc.Length
	if r.Loc.Offset < 0 || end > int64(len(b)) {
		t.Fatalf("Loc %+v outside the %s (%d bytes)", r.Loc, r.Loc.File, len(b))
	}
	if !bytes.Equal(b[r.Loc.Offset:end], want) {
		t.Errorf("the bytes at %+v are not the cell\n got %x\nwant %x", r.Loc, b[r.Loc.Offset:end], want)
	}
}

func intOf(t testing.TB, v sqlitefile.Value) int64 {
	t.Helper()
	if v.Kind != sqlitefile.KindInt {
		t.Fatalf("value %+v is not an integer", v)
	}
	return v.Int
}

func txtOf(t testing.TB, v sqlitefile.Value) string {
	t.Helper()
	s, ok := v.Text()
	if !ok {
		t.Fatalf("value %+v is not text", v)
	}
	return s
}

func methods(rows []sqlitefile.RecoveredRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Method)
	}
	return out
}

func hasNote(r sqlitefile.RecoveredRow, n string) bool {
	for _, x := range r.Notes {
		if x == n {
			return true
		}
	}
	return false
}

// TestHistoryRowsSupersededWALPage: a row updated twice in the WAL. The older
// committed frame yields the old values; the database image under the overlay
// yields the oldest. Rows identical to the live ones are not emitted.
func TestHistoryRowsSupersededWALPage(t *testing.T) {
	s := newRowScen(t)
	cell0 := s.cellT[2]
	s1, cell1, off1 := s.setT(2, 1)
	s2, _, _ := s.setT(2, 2)
	w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, s1.Page(2), 2)
	w.Frame(2, s2.Page(2), 2)
	f := files{db: walMode(s.db0), wal: w.Bytes()}
	_, h := openAll(t, f.db, f.wal, nil)
	rows, st := collectRows(t, h)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2: %v", len(rows), methods(rows))
	}
	old, mid := rows[0], rows[1] // origin order: the database image, then the frame
	for _, r := range rows {
		if r.Method != sqlitefile.MethodWALPrior || r.Table != "t" || r.Index != "" || r.TableBasis != sqlitefile.BasisSchema ||
			r.Relation != sqlitefile.RelSupersededVersion || r.Rowid == nil || *r.Rowid != 2 || r.Truncated {
			t.Errorf("row %+v", r)
		}
	}
	if old.Loc.File != sqlitefile.FileDB || old.Loc.Page != 2 || old.Loc.Offset != hps+int64(off1) {
		t.Errorf("oldest row Loc %+v", old.Loc)
	}
	if intOf(t, old.Values[0]) != 20 || txtOf(t, old.Values[1]) != tText(2, 0) {
		t.Errorf("oldest values %+v", old.Values)
	}
	assertCell(t, f, old, cell0)
	if mid.Loc.File != sqlitefile.FileWAL || mid.Loc.Frame != 1 || mid.Loc.Offset != walOff(1)+int64(off1) || mid.Loc.Length != int64(len(cell1)) {
		t.Errorf("frame row Loc %+v", mid.Loc)
	}
	if mid.WAL == nil || mid.WAL.Frame != 1 || mid.WAL.Salt1 != 0x1000 || mid.WAL.Salt2 != 0x1001 || !mid.WAL.Committed || mid.Journal != nil {
		t.Errorf("frame row provenance %+v", mid.WAL)
	}
	if intOf(t, mid.Values[0]) != 21 || txtOf(t, mid.Values[1]) != tText(2, 1) {
		t.Errorf("frame row values %+v", mid.Values)
	}
	assertCell(t, f, mid, cell1)
	if st.DuplicateOfLive != 4 || st.RowsByMethod[sqlitefile.MethodWALPrior] != 2 || st.Unknown != 0 || st.CellsRejected != 0 {
		t.Errorf("stats %+v", st)
	}
}

// TestHistoryRowsDuplicateOfLiveCounted: a page rewritten unchanged yields no row
// and every cell is counted as a duplicate of the live row.
func TestHistoryRowsDuplicateOfLiveCounted(t *testing.T) {
	s := newRowScen(t)
	w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, s.v0.Page(2), 2)
	_, h := openAll(t, walMode(s.db0), w.Bytes(), nil)
	rows, st := collectRows(t, h)
	if len(rows) != 0 || st.DuplicateOfLive != 3 {
		t.Errorf("%d rows, stats %+v: want none and 3 duplicates", len(rows), st)
	}
}

// TestHistoryRowsUncommittedWAL: an uncommitted frame yields its row with the
// relation uncommitted.
func TestHistoryRowsUncommittedWAL(t *testing.T) {
	s := newRowScen(t)
	s1, cell1, _ := s.setT(2, 1)
	w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, s.v0.Page(2), 2)
	w.Frame(2, s1.Page(2), 0)
	f := files{db: walMode(s.db0), wal: w.Bytes()}
	_, h := openAll(t, f.db, f.wal, nil)
	rows, st := collectRows(t, h)
	if len(rows) != 1 {
		t.Fatalf("%d rows: %v", len(rows), methods(rows))
	}
	r := rows[0]
	if r.Method != sqlitefile.MethodWALUncommitted || r.Relation != sqlitefile.RelUncommitted || r.WAL == nil || r.WAL.Committed || r.WAL.Frame != 2 ||
		r.TableBasis != sqlitefile.BasisSchema || intOf(t, r.Values[0]) != 21 {
		t.Errorf("row %+v wal %+v", r, r.WAL)
	}
	assertCell(t, f, r, cell1)
	if st.DuplicateOfLive != 5 {
		t.Errorf("duplicates %d, want 5 (3 under the overlay, 2 unchanged rows of the frame)", st.DuplicateOfLive)
	}
}

// TestHistoryRowsStaleGeneration: a frame of an older generation carries that
// generation's salts and the stale method.
func TestHistoryRowsStaleGeneration(t *testing.T) {
	s := newRowScen(t)
	s1, _, _ := s.setT(2, 1)
	s2, _, _ := s.setT(2, 2)
	s3, cell3, _ := s.setT(2, 3)
	s4, _, _ := s.setT(2, 4)
	w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, s1.Page(2), 2)
	w.Frame(2, s2.Page(2), 2)
	w.Frame(2, s3.Page(2), 2)
	w.Reset(0x2000, 0x2001)
	w.Frame(2, s4.Page(2), 2) // overwrites slot 1; slot 2 is stale and unlinked, slot 3 stale and linked to it
	f := files{db: walMode(s.db0), wal: w.Bytes()}
	_, h := openAll(t, f.db, f.wal, nil)
	rows, _ := collectRows(t, h)
	var stale []sqlitefile.RecoveredRow
	for _, r := range rows {
		if r.Method == sqlitefile.MethodWALStale {
			stale = append(stale, r)
		}
	}
	if len(stale) != 2 {
		t.Fatalf("%d stale rows in %v", len(stale), methods(rows))
	}
	if stale[0].WAL.Frame != 2 || stale[0].WAL.Linked {
		t.Errorf("slot 2 provenance %+v", stale[0].WAL)
	}
	r := stale[1]
	if r.WAL == nil || r.WAL.Salt1 != 0x1000 || r.WAL.Salt2 != 0x1001 || !r.WAL.Linked || r.WAL.Committed || r.WAL.Frame != 3 {
		t.Errorf("provenance %+v", r.WAL)
	}
	if r.Relation != sqlitefile.RelSupersededVersion || r.TableBasis != sqlitefile.BasisSchema || intOf(t, r.Values[0]) != 23 {
		t.Errorf("row %+v", r)
	}
	assertCell(t, f, r, cell3)
}

// rollbackScen: the database file holds an uncommitted transaction (t row 2 and
// u row 1 at version 5); the journal builder holds no record yet.
type rollbackScen struct {
	s       *rowScen
	db      []byte
	cellT   []byte // t row 2 as the database file holds it
	j       *sqlitetest.Journal
	before2 []byte // t's page before
	before3 []byte // u's page before
}

func newRollbackScen(t testing.TB) *rollbackScen {
	s := newRowScen(t)
	r := &rollbackScen{s: s, before2: s.v0.Page(2), before3: s.v0.Page(3)}
	s.setT(2, 5)
	s.setU(1, 5)
	r.cellT = s.cellOf(2)
	r.db = withCount(s.b.Bytes(), 3)
	r.j = s.b.NewJournal(512, 0xfeedbeef, 3)
	return r
}

// TestHistoryRowsDBRolledBack: a hot journal whose database file holds an
// uncommitted transaction. Live() shows the before-image rows; History delivers
// the as-found database cells as rolled back and uncommitted.
func TestHistoryRowsDBRolledBack(t *testing.T) {
	r := newRollbackScen(t)
	r.j.Record(2, r.before2)
	f := files{db: r.db, journal: r.j.Bytes()}
	d, h := openAll(t, f.db, nil, f.journal)
	if !d.Journal().Info.Applied {
		t.Fatal("journal not applied")
	}
	rows, st := collectRows(t, h)
	if len(rows) != 1 {
		t.Fatalf("%d rows: %v", len(rows), methods(rows))
	}
	row := rows[0]
	if row.Method != sqlitefile.MethodJournalRolledBack || row.Relation != sqlitefile.RelUncommitted || row.Loc.File != sqlitefile.FileDB ||
		row.Journal == nil || !row.Journal.Applied || row.WAL != nil || row.Table != "t" || intOf(t, row.Values[0]) != 25 {
		t.Errorf("row %+v journal %+v", row, row.Journal)
	}
	assertCell(t, f, row, r.cellT)
	if st.DuplicateOfLive != 5 { // 2 unchanged rows of page 2 in the file, 3 rows of the before-image
		t.Errorf("duplicates %d, want 5", st.DuplicateOfLive)
	}
	// the live rows lie in the journal record
	tb, err := d.Live().Table(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	live, ok, err := tb.Get(context.Background(), 2)
	if err != nil || !ok || live.Loc.File != sqlitefile.FileJournal || live.Loc.Record != 0 || intOf(t, live.Values[0]) != 20 {
		t.Errorf("live row %+v %v %v", live, ok, err)
	}
}

// TestHistoryRowsAppliedJournalBeforeImagesVanish: before-images of an applied
// journal are what Live() shows, so no row is emitted for them.
func TestHistoryRowsAppliedJournalBeforeImagesVanish(t *testing.T) {
	r := newRollbackScen(t)
	r.j.Record(2, r.before2)
	r.j.Record(3, r.before3)
	_, h := openAll(t, r.db, nil, r.j.Bytes())
	rows, st := collectRows(t, h)
	for _, row := range rows {
		if row.Method == sqlitefile.MethodJournalBefore {
			t.Errorf("an applied before-image row was emitted: %+v", row)
		}
	}
	if len(rows) != 2 || st.RowsByMethod[sqlitefile.MethodJournalRolledBack] != 2 || st.RowsByMethod[sqlitefile.MethodJournalBefore] != 0 {
		t.Errorf("%d rows %v, stats %+v", len(rows), methods(rows), st)
	}
	if st.DuplicateOfLive != 8 { // t: 2 + 3, u: 1 + 2 duplicates
		t.Errorf("duplicates %d", st.DuplicateOfLive)
	}
}

// TestHistoryRowsJournalAfterBadChecksum: records past the first bad checksum are
// not applied; their before-images differ from Live() and are delivered.
func TestHistoryRowsJournalAfterBadChecksum(t *testing.T) {
	r := newRollbackScen(t)
	r.j.Record(2, r.before2)
	r.j.RawRecord(3, r.before3, 1) // bad checksum
	f := files{db: r.db, journal: r.j.Bytes()}
	d, h := openAll(t, f.db, nil, f.journal)
	rows, _ := collectRows(t, h)
	var before []sqlitefile.RecoveredRow
	for _, row := range rows {
		if row.Method == sqlitefile.MethodJournalBefore {
			before = append(before, row)
		}
	}
	if len(before) != 1 {
		t.Fatalf("%d before-image rows in %v", len(before), methods(rows))
	}
	b := before[0]
	if b.Table != "u" || b.Journal == nil || b.Journal.ChecksumOK || b.Journal.Applied || b.Journal.Record != 1 || b.Relation != sqlitefile.RelSupersededVersion ||
		b.Loc.File != sqlitefile.FileJournal || b.Loc.Record != 1 || intOf(t, b.Values[0]) != 1 {
		t.Errorf("row %+v journal %+v", b, b.Journal)
	}
	if d.Journal().Records[1].ChecksumOK {
		t.Error("record 1 checksum is ok")
	}
}

// TestHistoryRowsColdJournalBeforeImages: a hot-flagged journal that names an
// unknown super-journal is not applied: its before-images are older versions of
// the live rows and are delivered with their relation from a strict compare.
func TestHistoryRowsColdJournalBeforeImages(t *testing.T) {
	s := newRowScen(t)
	cell0 := s.cellT[2]
	s.setT(2, 1)
	db := withCount(s.b.Bytes(), 3)
	j := s.b.NewJournal(512, 0xabcddcba, 3)
	j.Record(2, s.v0.Page(2))
	j.SuperJournal("gone-super-journal")
	f := files{db: db, journal: j.Bytes()}
	d, h := openAll(t, f.db, nil, f.journal)
	if in := d.Journal().Info; in.Applied || !in.Hot {
		t.Fatalf("journal %+v", in)
	}
	rows, _ := collectRows(t, h)
	if len(rows) != 1 {
		t.Fatalf("%d rows: %v", len(rows), methods(rows))
	}
	r := rows[0]
	if r.Method != sqlitefile.MethodJournalBefore || r.Relation != sqlitefile.RelSupersededVersion || r.Journal == nil || r.Journal.Applied ||
		!r.Journal.Hot || !r.Journal.ChecksumOK || intOf(t, r.Values[0]) != 20 || r.Loc.File != sqlitefile.FileJournal {
		t.Errorf("row %+v journal %+v", r, r.Journal)
	}
	assertCell(t, f, r, cell0)
}

// TestHistoryRowsJournalPersist: the records a PERSIST journal leaves behind
// (header zeroed after the commit) carry the persist method.
func TestHistoryRowsJournalPersist(t *testing.T) {
	s := newRowScen(t)
	s.setT(2, 1)
	db := withCount(s.b.Bytes(), 3)
	j := s.b.NewJournal(512, 0x1234abcd, 3)
	j.Record(2, s.v0.Page(2))
	j.Zero()
	f := files{db: db, journal: j.Bytes()}
	d, h := openAll(t, f.db, nil, f.journal)
	if in := d.Journal().Info; !in.ZeroedHeader || in.Applied {
		t.Fatalf("journal %+v", in)
	}
	rows, _ := collectRows(t, h)
	if len(rows) != 1 || rows[0].Method != sqlitefile.MethodJournalPersist || rows[0].Relation != sqlitefile.RelSupersededVersion {
		t.Fatalf("rows %+v", rows)
	}
}

// TestBasisSchemaNeedsStrictFit: an old image of a page that later belonged to a
// table its cells fit only loosely never gets BasisSchema, is never compared with
// the live owner's rows and carries owner-changed; one that fits strictly keeps
// BasisSchema.
func TestBasisSchemaNeedsStrictFit(t *testing.T) {
	build := func(oldCells func(z *sqlitetest.Table)) (db, wal []byte) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
		live := b.CreateTable("t", "create table t(a, b not null)")
		for id := int64(1); id <= 3; id++ {
			live.Insert(id, id, "live")
		}
		liveImg := b.Snapshot()
		db = walMode(withCount(b.Bytes(), liveImg.Pages()))
		// another database whose page 2 holds the old cells
		b2 := sqlitetest.New(sqlitetest.Options{PageSize: hps})
		z := b2.CreateTable("z", "create table z(x, y)")
		oldCells(z)
		old := b2.Snapshot()
		w := b.NewWAL(false, 0x1000, 0x1001, 0)
		w.Frame(2, old.Page(2), 2)
		w.Frame(2, liveImg.Page(2), 2)
		return db, w.Bytes()
	}
	t.Run("loose fit only", func(t *testing.T) {
		db, wal := build(func(z *sqlitetest.Table) {
			for id := int64(1); id <= 3; id++ {
				z.InsertRaw(id, []uint64{1}, []byte{byte(id)}) // one value: not enough for NOT NULL b
			}
		})
		_, h := openAll(t, db, wal, nil)
		rows, st := collectRows(t, h)
		if len(rows) != 3 || st.DuplicateOfLive != 3 { // the database image is the live page: 3 duplicates
			t.Fatalf("%d rows, stats %+v", len(rows), st)
		}
		for _, r := range rows {
			if r.TableBasis == sqlitefile.BasisSchema {
				t.Errorf("a loose fit got BasisSchema: %+v", r)
			}
			if r.Method == sqlitefile.MethodWALPrior && r.Loc.File == sqlitefile.FileWAL &&
				(r.TableBasis != sqlitefile.BasisGuess || r.Table != "t" || r.Relation != sqlitefile.RelUnknown || !hasNote(r, "owner-changed")) {
				t.Errorf("row %+v", r)
			}
		}
	})
	t.Run("strict fit", func(t *testing.T) {
		db, wal := build(func(z *sqlitetest.Table) {
			for id := int64(1); id <= 3; id++ {
				z.Insert(id, id*7, "old")
			}
		})
		_, h := openAll(t, db, wal, nil)
		rows, _ := collectRows(t, h)
		if len(rows) != 3 {
			t.Fatalf("%d rows", len(rows))
		}
		for _, r := range rows {
			if r.TableBasis != sqlitefile.BasisSchema || r.Table != "t" || r.Relation != sqlitefile.RelSupersededVersion || hasNote(r, "owner-changed") {
				t.Errorf("row %+v", r)
			}
		}
	})
}

// TestHistoryRowsAfterAddColumn: rows older than an ADD COLUMN are shorter than
// the table's columns and stay BasisSchema through the strict addable-column rule.
func TestHistoryRowsAfterAddColumn(t *testing.T) {
	old := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	ot := old.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 3; id++ {
		ot.Insert(id, id, fmt.Sprintf("b%d", id))
	}
	oldImg := old.Snapshot()

	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	nt := b.CreateTable("t", "create table t(a, b, c)")
	nt.Insert(1, int64(99), "b1", "new") // row 1 changed
	nt.Insert(2, int64(2), "b2", nil)
	nt.Insert(3, int64(3), "b3", nil)
	liveImg := b.Snapshot()
	db := walMode(withCount(b.Bytes(), liveImg.Pages()))
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, oldImg.Page(2), 2)
	w.Frame(2, liveImg.Page(2), 2)
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, st := collectRows(t, h)
	// the database image is the live page (3 duplicates); the old frame yields row 1 only
	if len(rows) != 1 || st.DuplicateOfLive != 5 {
		t.Fatalf("%d rows, stats %+v", len(rows), st)
	}
	r := rows[0]
	if r.TableBasis != sqlitefile.BasisSchema || r.Table != "t" || *r.Rowid != 1 || len(r.Values) != 2 || r.Relation != sqlitefile.RelSupersededVersion {
		t.Errorf("row %+v", r)
	}
}

// freelistRows returns the rows of the freelist-leaf images.
func freelistRows(rows []sqlitefile.RecoveredRow) []sqlitefile.RecoveredRow {
	var out []sqlitefile.RecoveredRow
	for _, r := range rows {
		if r.Method == sqlitefile.MethodFreelist {
			out = append(out, r)
		}
	}
	return out
}

// TestHistoryRowsFreelistLeaf: the leaf pages of a dropped table. A page whose
// cells fit exactly one live table strictly is named by BasisFit; a deleted
// table that no schema table fits is BasisNone; two tables of the same shape
// are BasisNone, never a pick.
func TestHistoryRowsFreelistLeaf(t *testing.T) {
	t.Run("one table fits", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
		live := b.CreateTable("t", "create table t(a, b)")
		for id := int64(1); id <= 3; id++ {
			live.Insert(id, id, "live")
		}
		pad := b.CreateTable("pad", "create table pad(z)")
		pad.Insert(1, "x") // dropped first: its page becomes the freelist trunk
		gone := b.CreateTable("gone", "create table gone(a, b)")
		for id := int64(10); id <= 12; id++ {
			gone.Insert(id, id, "gone-text")
		}
		gone4 := b.CreateTable("gone4", "create table gone4(p, q, r, s)")
		gone4.Insert(1, int64(1), "q", "r", int64(4))
		goneCell, _, _ := gone.CellBytes(10)
		b.DropTable("pad")
		b.DropTable("gone")
		b.DropTable("gone4")
		db := b.Bytes()
		f := files{db: db}
		_, h := openAll(t, db, nil, nil)
		rows, st := collectRows(t, h)
		fr := freelistRows(rows)
		if len(fr) != 4 || len(rows) != 4 {
			t.Fatalf("%d freelist rows of %d: %v", len(fr), len(rows), methods(rows))
		}
		var fit, none int
		for _, r := range fr {
			if r.Loc.File != sqlitefile.FileDB || r.WAL != nil || r.Journal != nil || r.Rowid == nil {
				t.Errorf("row %+v", r)
			}
			switch r.TableBasis {
			case sqlitefile.BasisFit:
				fit++
				if r.Table != "t" || r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteIdentityByFitOnly) || *r.Rowid < 10 {
					t.Errorf("fit row %+v", r)
				}
				if *r.Rowid == 10 {
					assertCell(t, f, r, goneCell)
				}
			case sqlitefile.BasisNone:
				none++
				if r.Table != "" || r.Index != "" || r.Relation != sqlitefile.RelUnknown {
					t.Errorf("unattributed row %+v", r)
				}
			default:
				t.Errorf("basis %s", r.TableBasis)
			}
		}
		if fit != 3 || none != 1 || st.Unknown != 4 || st.RowsByMethod[sqlitefile.MethodFreelist] != 4 {
			t.Errorf("fit %d none %d stats %+v", fit, none, st)
		}
	})
	t.Run("two tables of one shape", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
		p := b.CreateTable("p", "create table p(a, b)")
		q := b.CreateTable("q", "create table q(a, b)")
		p.Insert(1, int64(1), "p")
		q.Insert(1, int64(1), "q")
		pad := b.CreateTable("pad", "create table pad(z)")
		pad.Insert(1, "x")
		gone := b.CreateTable("gone", "create table gone(a, b)")
		gone.Insert(7, int64(7), "gone")
		b.DropTable("pad")
		b.DropTable("gone")
		_, h := openAll(t, b.Bytes(), nil, nil)
		rows, _ := collectRows(t, h)
		if len(rows) != 1 || rows[0].TableBasis != sqlitefile.BasisNone || rows[0].Table != "" || rows[0].Relation != sqlitefile.RelUnknown {
			t.Errorf("rows %+v", rows)
		}
	})
}

// TestHistoryRowsIndexEntries: entries of an index leaf carry the index (and its
// table), the rowid stays last in Values and the live state is not compared;
// interior cells are skipped and counted.
func TestHistoryRowsIndexEntries(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("i", "create table i(a, b)")
	for id := int64(1); id <= 300; id++ {
		tb.Insert(id, fmt.Sprintf("key-%05d-padpadpadpad", id), id)
	}
	b.CreateIndex("ia", "i", "create index ia on i(a)", 0)
	ix := b.Object("ia")
	if len(ix.Interiors()) != 1 || len(ix.Leaves()) < 3 {
		t.Fatalf("index tree: %d interiors %d leaves", len(ix.Interiors()), len(ix.Leaves()))
	}
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	tb.Update(150, fmt.Sprintf("key-%05d-padpadpadpad", 9150), int64(150)) // same length: the index entry moves
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	w.Frame(ix.Root(), v0.Page(ix.Root()), v0.Pages()) // an unchanged interior page
	f := files{db: db, wal: w.Bytes()}
	_, h := openAll(t, f.db, f.wal, nil)
	rows, st := collectRows(t, h)
	var idx, tbl []sqlitefile.RecoveredRow
	for _, r := range rows {
		if r.Index != "" {
			idx = append(idx, r)
		} else {
			tbl = append(tbl, r)
		}
	}
	if len(idx) == 0 || len(tbl) != 1 {
		t.Fatalf("%d index rows %d table rows", len(idx), len(tbl))
	}
	for _, r := range idx {
		if r.Index != "ia" || r.Table != "i" || r.TableBasis != sqlitefile.BasisSchema || r.Relation != sqlitefile.RelUnknown || r.Rowid != nil ||
			len(r.Values) != 2 || r.Values[1].Kind != sqlitefile.KindInt || r.Method != sqlitefile.MethodWALPrior {
			t.Errorf("index row %+v", r)
		}
	}
	// two images of the root: the database page and the older of the two frames
	if want := int64(2 * (len(ix.Leaves()) - 1)); st.InteriorSkipped != want {
		t.Errorf("interior cells skipped %d, want %d", st.InteriorSkipped, want)
	}
	if st.Unknown != int64(len(idx)) {
		t.Errorf("unknown %d, want %d", st.Unknown, len(idx))
	}
	// the old entry of row 150 is among them, rowid last
	found := false
	for _, r := range idx {
		if s, ok := r.Values[0].Text(); ok && s == fmt.Sprintf("key-%05d-padpadpadpad", 150) && r.Values[1].Int == 150 {
			found = true
		}
	}
	if !found {
		t.Error("the old index entry of row 150 is not delivered")
	}
}

// TestHistoryRowsWithoutRowid: a WITHOUT ROWID table is compared through a
// digest set of the live rows: unchanged rows are duplicates, a row whose key
// is live but whose values differ is a superseded version, a row whose key is
// gone is absent. Past MaxDiffRows the relation is unknown with limit-reached.
func TestHistoryRowsWithoutRowid(t *testing.T) {
	build := func() (db, wal []byte) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
		wr := b.CreateTableWithoutRowid("wr", "create table wr(k text primary key, v) without rowid", 1)
		for id := int64(1); id <= 3; id++ {
			wr.Insert(id, fmt.Sprintf("key%d", id), fmt.Sprintf("value-%d-v0", id))
		}
		v0 := b.Snapshot()
		db = walMode(withCount(b.Bytes(), v0.Pages()))
		wr.Update(2, "key2", "value-2-v1")
		wr.Delete(3)
		w := b.NewWAL(false, 0x1000, 0x1001, 0)
		b.CommitTo(w, v0)
		return db, w.Bytes()
	}
	db, wal := build()
	_, h := openAll(t, db, wal, nil)
	rows, st := collectRows(t, h)
	if len(rows) != 2 || st.DuplicateOfLive != 1 {
		t.Fatalf("%d rows, stats %+v", len(rows), st)
	}
	byKey := map[string]sqlitefile.RecoveredRow{}
	for _, r := range rows {
		byKey[txtOf(t, r.Values[0])] = r
	}
	if r := byKey["key2"]; r.Relation != sqlitefile.RelSupersededVersion || r.Table != "wr" || r.TableBasis != sqlitefile.BasisSchema || r.Rowid != nil {
		t.Errorf("key2 %+v", r)
	}
	if r := byKey["key3"]; r.Relation != sqlitefile.RelAbsentFromLive {
		t.Errorf("key3 %+v", r)
	}

	// the cap: more live rows than MaxDiffRows
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxDiffRows: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
		t.Fatal(err)
	}
	h2 := d.History()
	t.Cleanup(h2.Release)
	rows, st = collectRows(t, h2)
	if len(rows) != 3 || st.DuplicateOfLive != 0 || st.Unknown != 3 || !slices.Contains(st.LimitsHit, "MaxDiffRows") {
		t.Fatalf("capped: %d rows, stats %+v", len(rows), st)
	}
	for _, r := range rows {
		if r.Relation != sqlitefile.RelUnknown {
			t.Errorf("capped row %+v", r)
		}
	}
	if !hasCode(h2.Warnings(), sqlitefile.WarnLimitReached) {
		t.Errorf("no limit-reached warning: %v", h2.Warnings())
	}
}

// TestRecoveredRowCarriesItsOrigin: the page origin of the history is explicit
// on every row, next to its Loc.
func TestRecoveredRowCarriesItsOrigin(t *testing.T) {
	s := newRowScen(t)
	s1, _, _ := s.setT(2, 1)
	s2, _, _ := s.setT(2, 2)
	s3, _, _ := s.setT(2, 3)
	w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, s1.Page(2), 2)
	w.Frame(2, s2.Page(2), 2)
	w.Frame(2, s3.Page(2), 0) // uncommitted
	_, h := openAll(t, walMode(s.db0), w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	got := map[sqlitefile.Origin]bool{}
	for _, r := range rows {
		got[r.Origin] = true
	}
	for _, o := range []sqlitefile.Origin{sqlitefile.OriginDBUnderWAL, sqlitefile.OriginWALSuperseded, sqlitefile.OriginWALUncommitted} {
		if !got[o] {
			t.Errorf("no row with origin %s: %v", o, got)
		}
	}
	r := newRollbackScen(t)
	r.j.Record(2, r.before2)
	_, h = openAll(t, r.db, nil, r.j.Bytes())
	rows, _ = collectRows(t, h)
	if len(rows) != 1 || rows[0].Origin != sqlitefile.OriginDBRolledBack {
		t.Errorf("rolled-back row %+v", rows)
	}
}
