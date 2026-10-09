package sqlitedb_test

import (
	"encoding/hex"
	"fmt"
	"sort"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// TestRangeRolesAndOffsets compares the cell range and the role of every live
// row with the committed oracle (an independent decoder of the same files):
// the offset and length of the cell, the file it lies in, and the bytes found
// there. The WAL fixture holds every live cell of kv in the WAL; the hot
// journal fixture holds table a in the journal and table b in both the journal
// and the database.
func TestRangeRolesAndOffsets(t *testing.T) {
	roleOf := map[string]parse.FileRole{"db": parse.RoleDB, "wal": parse.RoleWAL, "journal": parse.RoleJournal}
	for _, fx := range []struct {
		name   string
		useWAL bool
		useJnl bool
	}{{"wal-uncheckpointed", true, false}, {"hot-journal", false, true}} {
		t.Run(fx.name, func(t *testing.T) {
			db, wal, journal, exp, err := readFixture(fixtureDir, fx.name)
			if err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{"db": db, "wal": wal, "journal": journal}
			if !fx.useWAL {
				wal = nil
			}
			if !fx.useJnl {
				journal = nil
			}
			d := openBytes(t, db, wal, journal, bigBudget())
			if len(exp.Cells) == 0 {
				t.Fatal("the oracle holds no cells")
			}
			names := make([]string, 0, len(exp.Cells))
			for n := range exp.Cells {
				names = append(names, n)
			}
			sort.Strings(names)
			seenFiles := map[string]int{}
			for _, name := range names {
				tb, err := d.Table(t.Context(), name, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				want := map[int64]fxCellRow{}
				for _, c := range exp.Cells[name] {
					want[c.Rowid] = c
				}
				got := 0
				scanEach(t, tb, func(_ int, r sqlitedb.Row) {
					got++
					id, _ := r.Rowid()
					c, ok := want[id]
					if !ok {
						t.Errorf("%s rowid %d is not in the oracle", name, id)
						return
					}
					rng, role := r.Range()
					if role != roleOf[c.File] || rng.Offset != c.Offset || rng.Length != c.Length {
						t.Errorf("%s rowid %d: Range = %+v %q, oracle %s %d+%d", name, id, rng, role, c.File, c.Offset, c.Length)
					}
					f := files[c.File]
					if rng.Offset < 0 || rng.Offset+rng.Length > int64(len(f)) {
						t.Fatalf("%s rowid %d: range outside the %s file", name, id, c.File)
					}
					if hex.EncodeToString(f[rng.Offset:rng.Offset+rng.Length]) != c.CellHex {
						t.Errorf("%s rowid %d: the bytes of the range are not the oracle's cell", name, id)
					}
					seenFiles[c.File]++
					l := r.Loc()
					if l.Offset != rng.Offset || l.Length != rng.Length {
						t.Errorf("%s rowid %d: Loc %d+%d, Range %d+%d", name, id, l.Offset, l.Length, rng.Offset, rng.Length)
					}
				})
				if got != len(want) {
					t.Errorf("%s: %d live rows, the oracle has %d cells", name, got, len(want))
				}
			}
			switch fx.name {
			case "wal-uncheckpointed":
				if seenFiles["wal"] == 0 || seenFiles["journal"] != 0 {
					t.Errorf("files seen: %v", seenFiles)
				}
			case "hot-journal":
				if seenFiles["journal"] == 0 || seenFiles["db"] == 0 {
					t.Errorf("files seen: %v; the fixture should mix journal and database cells", seenFiles)
				}
			}
		})
	}
}

// TestLocatorsOfWALAndJournalCells checks the frame and record numbers of the
// locator against the oracle's frame and record tables, found by position.
func TestLocatorsOfWALAndJournalCells(t *testing.T) {
	t.Run("wal frame", func(t *testing.T) {
		db, wal, _, exp, err := readFixture(fixtureDir, "wal-uncheckpointed")
		if err != nil {
			t.Fatal(err)
		}
		ps := int64(exp.WAL.PageSize)
		d := openBytes(t, db, wal, nil, bigBudget())
		tb, err := d.Table(t.Context(), "kv", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			rng, _ := r.Range()
			slot := uint32(0)
			for _, f := range exp.WAL.Frames {
				if rng.Offset >= f.Offset+24 && rng.Offset < f.Offset+24+ps {
					slot = f.Slot
				}
			}
			id, _ := r.Rowid()
			want := fmt.Sprintf("sqlite:table=kv;rowid=%d;wal_frame=%d", id, slot)
			if got, ok := r.Locator(); !ok || slot == 0 || got != want {
				t.Fatalf("rowid %d: Locator = %q, %v; want %q", id, got, ok, want)
			}
			n++
		})
		if n != 197 {
			t.Errorf("%d rows", n)
		}
	})
	t.Run("journal record", func(t *testing.T) {
		db, _, journal, exp, err := readFixture(fixtureDir, "hot-journal")
		if err != nil {
			t.Fatal(err)
		}
		ps := int64(exp.Journal.PageSize)
		d := openBytes(t, db, nil, journal, bigBudget())
		sawJournal, sawDB := 0, 0
		for _, name := range []string{"a", "b"} {
			tb, err := d.Table(t.Context(), name, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := map[int64]fxCellRow{}
			for _, c := range exp.Cells[name] {
				want[c.Rowid] = c
			}
			scanEach(t, tb, func(_ int, r sqlitedb.Row) {
				id, _ := r.Rowid()
				c := want[id]
				got, ok := r.Locator()
				if !ok {
					t.Fatalf("%s rowid %d: no locator", name, id)
				}
				if c.File == "db" {
					sawDB++
					if exp := fmt.Sprintf("sqlite:table=%s;rowid=%d", name, id); got != exp {
						t.Errorf("Locator = %q, want %q", got, exp)
					}
					return
				}
				sawJournal++
				idx := -1
				for _, rec := range exp.Journal.Records {
					// a record is a 4-byte page number then the page image
					if rec.Page == c.Page && c.Offset >= rec.Offset+4 && c.Offset < rec.Offset+4+ps {
						idx = rec.Index
					}
				}
				if idx < 0 {
					t.Fatalf("%s rowid %d: no oracle record holds the cell", name, id)
				}
				if exp := fmt.Sprintf("sqlite:table=%s;rowid=%d;journal_rec=%d", name, id, idx); got != exp {
					t.Errorf("Locator = %q, want %q", got, exp)
				}
			})
		}
		if sawJournal == 0 || sawDB == 0 {
			t.Errorf("journal rows %d, database rows %d", sawJournal, sawDB)
		}
	})
}
