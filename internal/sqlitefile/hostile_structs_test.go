package sqlitefile_test

// Hostile structures (plan 3I, Task 15): files whose structure lies (cycles,
// pointers to the wrong kind of page, schema rows with impossible root pages,
// companion files that do not match the database). Each case terminates with
// bounded Stats and its expected warning, and the rows of an undamaged table
// are what a clean file gives.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const hsPS = 512

// hsBuilder: table "good" on page 2 (undamaged), table "victim" on page 3.
func hsBuilder(victimRows int) (*sqlitetest.Builder, *sqlitetest.Table, *sqlitetest.Table) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hsPS})
	good := b.CreateTable("good", "create table good(a, b)")
	for id := int64(1); id <= 5; id++ {
		good.Insert(id, id*3, fmt.Sprintf("good-%d", id))
	}
	victim := b.CreateTable("victim", "create table victim(a, b)")
	for id := int64(1); id <= int64(victimRows); id++ {
		victim.Insert(id, id, fmt.Sprintf("victim-%d-padpadpadpadpadpad", id))
	}
	return b, good, victim
}

// goodRows reads the rows of the table "good" from the live view as strings.
func goodRows(t *testing.T, d *sqlitefile.DB, asFound bool) []string {
	t.Helper()
	v := d.Live()
	if asFound {
		v = d.AsFound()
	}
	defer v.Release()
	tb, err := v.Table(context.Background(), "good")
	if err != nil {
		t.Fatalf("Table good: %v", err)
	}
	var out []string
	if err := tb.Rows(context.Background(), func(r sqlitefile.Row) bool {
		out = append(out, fmt.Sprintf("%d:%s", r.Rowid, hsRowText(r)))
		return true
	}); err != nil {
		t.Fatalf("Rows good: %v", err)
	}
	return out
}

func hsRowText(r sqlitefile.Row) string {
	var s string
	for _, v := range r.Values {
		switch v.Kind {
		case sqlitefile.KindInt:
			s += fmt.Sprintf("%d,", v.Int)
		case sqlitefile.KindText:
			s += string(v.Bytes) + ","
		default:
			s += "?,"
		}
	}
	return s
}

func allWarningCodes(d *sqlitefile.DB, h *sqlitefile.Hist) []string {
	var codes []string
	for _, w := range d.Warnings() {
		codes = append(codes, w.Code)
	}
	for _, w := range h.Warnings() {
		codes = append(codes, w.Code)
	}
	if s := d.WAL(); s != nil {
		for _, w := range s.Warnings {
			codes = append(codes, w.Code)
		}
	}
	if s := d.Journal(); s != nil {
		for _, w := range s.Warnings {
			codes = append(codes, w.Code)
		}
	}
	return codes
}

func TestHostileStructures(t *testing.T) {
	cleanB, _, _ := hsBuilder(8)
	clean := cleanB.Bytes()
	cd, err := sqlitefile.Open(bytes.NewReader(clean), int64(len(clean)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wantGood := goodRows(t, cd, false)
	if len(wantGood) != 5 {
		t.Fatalf("clean good rows: %v", wantGood)
	}

	type tcase struct {
		name  string
		files func(t *testing.T) hostileFiles
		// codes: at least one of these warning codes must be raised
		codes []string
		// problem: or a Layout problem containing this text; wantErr: or a
		// lookup that fails with ErrNotFound (a table with no b-tree)
		problem string
		quiet   bool // a sound file: no warning is expected, only the bounds
		asFound bool // the live state is refused: the rows of "good" are read as found
		wantErr bool
		// goodSurvives: the rows of "good" are read as in the clean file
		goodMissing bool
	}
	big := func() (*sqlitetest.Builder, *sqlitetest.Table, *sqlitetest.Table) { return hsBuilder(60) }
	cases := []tcase{
		{name: "a freelist cycle", files: func(*testing.T) hostileFiles {
			b, _, _ := hsBuilder(8)
			b.SetFreelist([][]uint32{{4}, {5}})
			db := b.Bytes()
			binary.BigEndian.PutUint32(pageAt(db, hsPS, 5)[0:], 4) // trunk 5 points back at trunk 4
			return hostileFiles{db: db}
		}, codes: []string{sqlitefile.WarnFreelistCycle}},
		{name: "a trunk pointing at a b-tree page", files: func(*testing.T) hostileFiles {
			b, _, _ := hsBuilder(8)
			b.SetFreelist([][]uint32{{4}})
			db := b.Bytes()
			binary.BigEndian.PutUint32(db[32:], 3) // the first trunk is the victim's leaf
			return hostileFiles{db: db}
		}, codes: []string{sqlitefile.WarnFreelistCycle, sqlitefile.WarnFreelistCount, sqlitefile.WarnFreelistLeafCount, sqlitefile.WarnPageTypeInvalid, sqlitefile.WarnPtrmapMismatch}},
		{name: "a b-tree pointing at the freelist", files: func(*testing.T) hostileFiles {
			b, _, victim := big()
			b.Free(2 + uint32(len(b.Bytes())/hsPS)) // one free page past the end
			db := b.Bytes()
			setChild(db, hsPS, victim.Root(), 0, uint32(len(db)/hsPS))
			return hostileFiles{db: db}
		}, codes: []string{sqlitefile.WarnBTreeShape, sqlitefile.WarnPageTypeInvalid, sqlitefile.WarnBTreeCycle, sqlitefile.WarnPageRange}},
		{name: "a schema row with root page 0", files: func(*testing.T) hostileFiles {
			b, _, _ := hsBuilder(8)
			b.AddSchemaRow("table", "bad0", "bad0", int64(0), "create table bad0(x)")
			return hostileFiles{db: b.Bytes()}
		}, wantErr: true},
		{name: "a schema row with root page 1", files: func(*testing.T) hostileFiles {
			b, _, _ := hsBuilder(8)
			b.AddSchemaRow("table", "bad1", "bad1", int64(1), "create table bad1(x)")
			return hostileFiles{db: b.Bytes()}
		}, problem: "page 1 is claimed twice"},
		{name: "a schema row with a root page past the end", files: func(*testing.T) hostileFiles {
			b, _, _ := hsBuilder(8)
			b.AddSchemaRow("table", "badx", "badx", int64(99999), "create table badx(x)")
			return hostileFiles{db: b.Bytes()}
		}, codes: []string{sqlitefile.WarnSchemaRowInvalid, sqlitefile.WarnPageRange, sqlitefile.WarnPageUnavailable}},
		{name: "two tables with the same root page", files: func(*testing.T) hostileFiles {
			b, _, _ := hsBuilder(8)
			b.AddSchemaRow("table", "twin", "twin", int64(2), "create table twin(a, b)")
			return hostileFiles{db: b.Bytes()}
		}, problem: "page 2 is claimed twice"},
		{name: "page 1 an interior page with a cycle through page 1", files: func(t *testing.T) hostileFiles {
			b := sqlitetest.New(sqlitetest.Options{PageSize: hsPS})
			good := b.CreateTable("good", "create table good(a, b)")
			for id := int64(1); id <= 5; id++ {
				good.Insert(id, id*3, fmt.Sprintf("good-%d", id))
			}
			for i := 0; i < 40; i++ {
				b.CreateTable(fmt.Sprintf("pad%02d", i), fmt.Sprintf("create table pad%02d(a, b, c)", i))
			}
			db := b.Bytes()
			if db[100] != 0x05 {
				t.Fatalf("page 1 is type %#x, want an interior table page", db[100])
			}
			// the first child pointer of page 1 points at page 1
			off := int(binary.BigEndian.Uint16(db[100+12:])) // the first cell pointer of an interior page
			binary.BigEndian.PutUint32(db[off:], 1)
			return hostileFiles{db: db}
		}, codes: []string{sqlitefile.WarnPageRange}, problem: "page 1 is claimed twice", goodMissing: true},
		{name: "a schema row naming a pointer-map page", files: func(*testing.T) hostileFiles {
			b := sqlitetest.New(sqlitetest.Options{PageSize: hsPS, AutoVacuum: 1})
			good := b.CreateTable("good", "create table good(a, b)")
			for id := int64(1); id <= 5; id++ {
				good.Insert(id, id*3, fmt.Sprintf("good-%d", id))
			}
			b.WritePtrmap()
			b.AddSchemaRow("table", "pm", "pm", int64(2), "create table pm(x)")
			return hostileFiles{db: b.Bytes()}
		}, problem: "page 2 is claimed twice: it is a pointer-map page", goodMissing: true},
		{name: "a WAL in which every frame is the same page", files: func(*testing.T) hostileFiles {
			b, _, victim := hsBuilder(8)
			db := walMode(withCount(b.Bytes(), b.Snapshot().Pages()))
			w := b.NewWAL(false, 1, 2, 0)
			for i := 0; i < 60; i++ {
				victim.Update(1, int64(i), "same-page")
				w.Frame(victim.Root(), b.Snapshot().Page(victim.Root()), uint32(len(db)/hsPS))
			}
			return hostileFiles{db: db, wal: w.Bytes()}
		}, quiet: true},
		{name: "a WAL with a valid chain and page size 512 on a 64 KiB database", files: func(*testing.T) hostileFiles {
			big := sqlitetest.New(sqlitetest.Options{PageSize: 65536})
			g := big.CreateTable("good", "create table good(a, b)")
			for id := int64(1); id <= 5; id++ {
				g.Insert(id, id*3, fmt.Sprintf("good-%d", id))
			}
			db := walMode(withCount(big.Bytes(), big.Snapshot().Pages()))
			small := sqlitetest.New(sqlitetest.Options{PageSize: 512})
			w := small.NewWAL(false, 1, 2, 0)
			w.Frame(2, make([]byte, 512), 3)
			w.Frame(3, make([]byte, 512), 3)
			return hostileFiles{db: db, wal: w.Bytes()}
		}, codes: []string{sqlitefile.WarnWALPageSizeMismatch}, asFound: true, goodMissing: true},
		{name: "journal records for page 0 and for the lock-byte page", files: func(*testing.T) hostileFiles {
			b, _, _ := hsBuilder(8)
			img := b.Snapshot()
			db := withCount(b.Bytes(), img.Pages())
			j := b.NewJournal(512, 5, img.Pages())
			j.Record(0, img.Page(2))
			j.Record(uint32((1<<30)/hsPS+1), img.Page(2))
			j.Record(2, img.Page(2))
			return hostileFiles{db: db, journal: j.Bytes()}
		}, codes: []string{sqlitefile.WarnJournalPageInvalid}},
		{name: "a journal with sector size 0", files: func(*testing.T) hostileFiles {
			b, _, _ := hsBuilder(8)
			img := b.Snapshot()
			db := withCount(b.Bytes(), img.Pages())
			j := b.NewJournal(512, 5, img.Pages())
			j.Record(2, img.Page(2))
			j.SetHeader(1, 5, img.Pages(), 0, hsPS)
			return hostileFiles{db: db, journal: j.Bytes()}
		}, codes: []string{sqlitefile.WarnJournalSectorInvalid, sqlitefile.WarnJournalHeaderInvalid}},
		{name: "a cell pointer array that overlaps the content area", files: func(*testing.T) hostileFiles {
			b, _, victim := hsBuilder(8)
			db := b.Bytes()
			leaf := pageAt(db, hsPS, victim.Root())
			binary.BigEndian.PutUint16(leaf[3:], 240) // 240 pointers: the array runs into the cells
			return hostileFiles{db: db}
		}, codes: []string{sqlitefile.WarnCellPointer, sqlitefile.WarnPageTypeInvalid, sqlitefile.WarnBTreeShape}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.files(t)
			rb := newRecBudget(64 << 20)
			d, err := sqlitefile.Open(newCountReader(f.db), int64(len(f.db)), sqlitefile.Options{Budget: rb})
			noPanic(t, "Open", err)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if f.wal != nil {
				_, err := d.AttachWAL(bytes.NewReader(f.wal), int64(len(f.wal)))
				noPanic(t, "AttachWAL", err)
			}
			if f.journal != nil {
				_, err := d.AttachJournal(bytes.NewReader(f.journal), int64(len(f.journal)))
				noPanic(t, "AttachJournal", err)
			}
			res := exercise(t, f, newRecBudget(64<<20))
			h := d.History()
			_, _ = h.Rows(context.Background(), func(sqlitefile.RecoveredRow) bool { return true })
			codes := allWarningCodes(d, h)
			var problems []string
			if lv := d.Live(); true {
				if lay, err := lv.Layout(context.Background()); err == nil {
					problems = lay.Problems
				}
				lv.Release()
			}
			h.Release()
			t.Logf("warnings %v; layout problems %q; errors %d; stats %+v; rows %d", dedup(codes), problems, len(res.errs), res.stats, res.rows)
			ok := slices.ContainsFunc(codes, func(c string) bool { return slices.Contains(tc.codes, c) }) ||
				(tc.problem != "" && slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.problem) })) ||
				(tc.wantErr && len(res.errs) > 0)
			if !ok && !tc.quiet {
				t.Errorf("expected one of the warnings %v, the layout problem %q or an error (%v); got warnings %v, problems %q, %d errors", tc.codes, tc.problem, tc.wantErr, dedup(codes), problems, len(res.errs))
			}
			total := int64(f.total())
			pages := total/hsPS + 2
			if res.stats.PageReads > 4*pages || res.stats.CellsParsed > total || res.stats.PagesSkipped > 4*pages {
				t.Errorf("stats %+v not bounded by the %d real bytes", res.stats, total)
			}
			if !tc.goodMissing {
				if got := goodRows(t, d, tc.asFound); !slices.Equal(got, wantGood) {
					t.Errorf("rows of the undamaged table: %v, want %v", got, wantGood)
				}
			}
		})
	}
}

func dedup(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}
