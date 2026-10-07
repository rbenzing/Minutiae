package sqlitefile_test

// Engine oracle, history, part 2 (plan 3I, Task 13): secure_delete, VACUUM, the
// confidence tiers and files the engine refuses.

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func vacuumScenario(t *testing.T) *histFixture {
	g, path := newHistGen(t, "vacuum", 1024, "delete")
	mustExec(t, g.db, "pragma secure_delete=off")
	g.exec("create table t(a integer, b text)")
	g.begin()
	for id := int64(1); id <= 400; id++ {
		g.ins("t", id, 0, false)
	}
	g.commit()
	g.begin()
	for id := int64(1); id <= 330; id++ {
		g.del("t", id)
	}
	g.commit()
	g.exec("vacuum")
	_ = g.db.Close()
	return finishFixture(t, &histFixture{name: "vacuum", db: readAll(t, path), log: g.log})
}

func deletedTexts(f *histFixture) []string {
	live := liveTexts(f.live)
	var out []string
	for s := range f.log.byText {
		if !live[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// TestSecureDeleteInventsNothing: with secure_delete=ON the deleted markers are
// in no file, hence in no recovered row and no span; the freed leaf pages read
// zero and are counted FreePagesZeroed.
func TestSecureDeleteInventsNothing(t *testing.T) {
	f := scenDeleteRollback(t, true)
	gone := deletedTexts(f)
	if len(gone) < 300 {
		t.Fatalf("%d deleted markers", len(gone))
	}
	for _, s := range gone {
		if bytes.Contains(f.db, []byte(s)) {
			t.Fatalf("the engine left %q in the file despite secure_delete: the scenario is vacuous", s)
		}
	}
	d := openLibrary(t, f.db, nil, nil)
	rows, spans, _, sum := collectHistory(t, d)
	live := liveTexts(f.live)
	for _, r := range rows {
		if _, ok := f.log.byText[rowText(r)]; ok && !live[rowText(r)] {
			t.Errorf("a deleted marker was recovered under secure_delete: %s", rowText(r))
		}
	}
	for _, sp := range spans[sqlitefile.FileDB] {
		seg := f.db[sp.FileOffset : sp.FileOffset+int64(sp.Length)]
		for _, s := range gone {
			if bytes.Contains(seg, []byte(s)) {
				t.Errorf("a span at %d holds the deleted marker %q", sp.FileOffset, s)
			}
		}
	}
	e := openEngine(t, writeTemp(t, f.db))
	free := pragmaString(t, e, "freelist_count")
	if sum.FreePages == 0 || fmt.Sprint(sum.FreePages) != free {
		t.Errorf("FreePages %d, engine freelist_count %s", sum.FreePages, free)
	}
	if sum.FreePagesZeroed == 0 || sum.FreePagesZeroed >= sum.FreePages {
		t.Errorf("FreePagesZeroed %d of %d free pages: every freed leaf reads zero, the trunk does not", sum.FreePagesZeroed, sum.FreePages)
	}
}

// TestVacuumLeavesNothing: after VACUUM the freelist is empty and the deleted
// markers are nowhere.
func TestVacuumLeavesNothing(t *testing.T) {
	f := vacuumScenario(t)
	for _, s := range deletedTexts(f) {
		if bytes.Contains(f.db, []byte(s)) {
			t.Fatalf("VACUUM left %q in the file: the scenario is vacuous", s)
		}
	}
	d := openLibrary(t, f.db, nil, nil)
	if d.Info().FreelistCount != 0 {
		t.Errorf("freelist count %d after VACUUM", d.Info().FreelistCount)
	}
	rows, _, _, sum := collectHistory(t, d)
	if len(rows) != 0 || sum.FreePages != 0 {
		t.Errorf("%d recovered rows, %d free pages after VACUUM", len(rows), sum.FreePages)
	}
}

// TestFitTiersProveConfidenceNumbers (D2): each cap of the confidence rule
// appears on a row the engine produced.
func TestFitTiersProveConfidenceNumbers(t *testing.T) {
	t.Run("fit: 60 on a freelist row of the only table of its shape", func(t *testing.T) {
		rows := checkHistory(t, scenDeleteRollback(t, false))
		for _, r := range rows {
			if r.TableBasis != sqlitefile.BasisFit || r.Table != "t" || sqlitefile.Confidence(r) != 60 {
				t.Fatalf("row basis %s table %q confidence %d", r.TableBasis, r.Table, sqlitefile.Confidence(r))
			}
		}
	})
	t.Run("fit caps a wal-prior row at 60 and says the table rests on the fit alone", func(t *testing.T) {
		// g is dropped and t (the same shape) is created on its freed pages: the
		// old frames of g fit t strictly and uniquely, but the page was g's when
		// they were written. The label is the fit's, the relation unknown.
		g, path := newHistGen(t, "reuse", 1024, "wal")
		g.exec("create table g(a integer, b text)")
		g.begin()
		for id := int64(1); id <= 60; id++ {
			g.ins("g", id, 0, false)
		}
		g.commit()
		g.exec("drop table g")
		g.exec("create table t(a integer, b text)")
		g.begin()
		for id := int64(1); id <= 5; id++ {
			g.ins("t", id, 0, false)
		}
		g.commit()
		f := copyWALFixture(t, "reuse", path, g, nil)
		_ = g.db.Close()
		d := openLibrary(t, f.db, f.wal, nil)
		rows, _, _, _ := collectHistory(t, d)
		n := 0
		for _, r := range rows {
			if r.Method != sqlitefile.MethodWALPrior || !strings.Contains(rowText(r), "-g-") {
				continue
			}
			n++
			if r.TableBasis == sqlitefile.BasisSchema {
				t.Errorf("a row of the dropped table g is BasisSchema of %q", r.Table)
			}
			if r.TableBasis == sqlitefile.BasisFit {
				if sqlitefile.Confidence(r) != 60 || r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteIdentityByFitOnly) {
					t.Errorf("fit row: confidence %d relation %s notes %v", sqlitefile.Confidence(r), r.Relation, r.Notes)
				}
			}
		}
		if n == 0 {
			t.Fatal("no row of g was delivered")
		}
	})
	t.Run("guess: 40 on a freelist row that fits only loosely", func(t *testing.T) {
		g, path := newHistGen(t, "guess", 1024, "delete")
		g.exec("create table g(a integer, b text, c integer)")
		g.exec("create table pad(z)")
		g.begin()
		g.exec("insert into pad values(1)")
		for id := int64(1); id <= 40; id++ {
			a, b := id*1000, marker("g", id, 0)
			g.exec("insert into g(rowid,a,b,c) values(?,?,?,null)", id, a, b)
			g.log.add("g", id, a, b, nil)
		}
		g.commit()
		g.exec("drop table pad")
		g.exec("drop table g")
		g.exec("create table t(a integer, b text, c integer not null default 0)")
		_ = g.db.Close()
		f := finishFixture(t, &histFixture{name: "guess", db: readAll(t, path), log: g.log})
		d := openLibrary(t, f.db, nil, nil)
		rows, _, _, _ := collectHistory(t, d)
		n := 0
		for _, r := range rows {
			if r.Method != sqlitefile.MethodFreelist {
				continue
			}
			n++
			if r.TableBasis != sqlitefile.BasisGuess || sqlitefile.Confidence(r) != 40 {
				t.Errorf("row basis %s table %q confidence %d, want guess and 40", r.TableBasis, r.Table, sqlitefile.Confidence(r))
			}
		}
		if n == 0 {
			t.Fatal("no freelist row")
		}
	})
	t.Run("none: 30 on rows of a table whose shape two tables share", func(t *testing.T) {
		rows := checkHistory(t, scenDrop(t))
		for _, r := range rows {
			if r.Method == sqlitefile.MethodFreelist && (r.TableBasis != sqlitefile.BasisNone || sqlitefile.Confidence(r) != 30) {
				t.Fatalf("row basis %s confidence %d", r.TableBasis, sqlitefile.Confidence(r))
			}
		}
	})
	t.Run("45: a rolled-back row", func(t *testing.T) {
		rows := checkHistory(t, scenHotJournal(t))
		seen := false
		for _, r := range rows {
			if r.Method == sqlitefile.MethodJournalRolledBack && r.TableBasis == sqlitefile.BasisSchema {
				seen = true
				if sqlitefile.Confidence(r) != 45 {
					t.Fatalf("rolled-back confidence %d", sqlitefile.Confidence(r))
				}
			}
		}
		if !seen {
			t.Fatal("no rolled-back row")
		}
	})
}

// TestEncryptedLooksInvalidAgainstEngine: a file the engine itself refuses
// ("file is not a database") is ErrNotSQLite for the library, and a 4 KiB
// random file is ErrLooksEncrypted.
func TestEncryptedLooksInvalidAgainstEngine(t *testing.T) {
	random := make([]byte, 4096)
	x := uint64(0x9e3779b97f4a7c15)
	for i := range random {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		random[i] = byte(x >> 24)
	}
	text := bytes.Repeat([]byte("this is not a database, just text\n"), 200)
	for _, c := range []struct {
		name      string
		data      []byte
		encrypted bool
	}{{"random 4 KiB", random, true}, {"text", text, false}} {
		e := openEngine(t, writeTemp(t, c.data))
		var n int
		err := e.QueryRow("select count(*) from sqlite_schema").Scan(&n)
		if err == nil || !strings.Contains(err.Error(), "not a database") {
			t.Fatalf("%s: the engine says %v", c.name, err)
		}
		_, oerr := sqlitefile.Open(bytes.NewReader(c.data), int64(len(c.data)), sqlitefile.Options{})
		if !errors.Is(oerr, sqlitefile.ErrNotSQLite) {
			t.Errorf("%s: library error %v, want ErrNotSQLite", c.name, oerr)
		}
		if got := errors.Is(oerr, sqlitefile.ErrLooksEncrypted); got != c.encrypted {
			t.Errorf("%s: ErrLooksEncrypted = %v, want %v", c.name, got, c.encrypted)
		}
	}
}
