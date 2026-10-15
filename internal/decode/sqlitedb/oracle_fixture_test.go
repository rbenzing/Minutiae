package sqlitedb_test

// Fixture-oracle tests of the row layer. The committed oracles come from an
// independent decoder of the same files (internal/sqlitefile/testdata), so these
// tests compare the layer with a source that shares no code with it.

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// fixtureNames lists every committed fixture.
func fixtureNames(t testing.TB) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(fixtureDir, "*.expect.json"))
	if err != nil || len(m) == 0 {
		t.Fatalf("no fixtures in %s: %v", fixtureDir, err)
	}
	out := make([]string, 0, len(m))
	for _, p := range m {
		out = append(out, strings.TrimSuffix(filepath.Base(p), ".expect.json"))
	}
	sort.Strings(out)
	return out
}

// TestFixtureRangesEqualTheOracle: the range and role of every live row equal
// the oracle's cell for the same rowid (file, page, offset, length), and the
// bytes of the artifact at that range are the oracle's cell, for database, WAL
// and journal cells alike.
func TestFixtureRangesEqualTheOracle(t *testing.T) {
	roleOf := map[string]parse.FileRole{"db": parse.RoleDB, "wal": parse.RoleWAL, "journal": parse.RoleJournal}
	fileOf := map[sqlitefile.FileKind]string{sqlitefile.FileDB: "db", sqlitefile.FileWAL: "wal", sqlitefile.FileJournal: "journal"}
	seenRoles := map[string]int{}
	compared := 0
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db, wal, journal, exp, err := readFixture(fixtureDir, name)
			if err != nil {
				t.Fatal(err)
			}
			if exp.NotSQLite || len(exp.Cells) == 0 {
				t.Skip("the oracle lists no cells")
			}
			files := map[string][]byte{"db": db, "wal": wal, "journal": journal}
			d := openBytes(t, db, wal, journal, bigBudget())
			tables := make([]string, 0, len(exp.Cells))
			for n := range exp.Cells {
				tables = append(tables, n)
			}
			sort.Strings(tables)
			for _, tn := range tables {
				tb, err := d.Table(t.Context(), tn, nil, nil)
				if err != nil {
					t.Fatalf("table %s: %v", tn, err)
				}
				want := map[int64]fxCellRow{}
				for _, c := range exp.Cells[tn] {
					want[c.Rowid] = c
				}
				n := 0
				if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
					n++
					id, ok := r.Rowid()
					if !ok {
						return nil
					}
					c, ok := want[id]
					if !ok {
						t.Errorf("%s rowid %d: no such cell in the oracle", tn, id)
						return nil
					}
					rng, role := r.Range()
					if role != roleOf[c.File] {
						t.Errorf("%s rowid %d: role %q, the oracle's cell lies in the %s", tn, id, role, c.File)
					}
					if rng.Offset != c.Offset || rng.Length != c.Length {
						t.Errorf("%s rowid %d: range %d+%d, oracle %d+%d", tn, id, rng.Offset, rng.Length, c.Offset, c.Length)
					}
					l := r.Loc()
					if l.Page != c.Page || fileOf[l.File] != c.File {
						t.Errorf("%s rowid %d: loc page %d in %s, oracle page %d in %s", tn, id, l.Page, fileOf[l.File], c.Page, c.File)
					}
					f := files[c.File]
					if rng.Offset < 0 || rng.Length < 0 || rng.Offset+rng.Length > int64(len(f)) {
						t.Errorf("%s rowid %d: range outside the %s artifact (%d bytes)", tn, id, c.File, len(f))
						return nil
					}
					if hex.EncodeToString(f[rng.Offset:rng.Offset+rng.Length]) != c.CellHex {
						t.Errorf("%s rowid %d: the bytes at the range are not the oracle's cell", tn, id)
					}
					seenRoles[c.File]++
					compared++
					return nil
				}); err != nil {
					t.Fatalf("Scan %s: %v", tn, err)
				}
				if n == 0 {
					t.Errorf("table %s: no live row, nothing was compared", tn)
				}
			}
		})
	}
	if compared == 0 || seenRoles["db"] == 0 || seenRoles["wal"] == 0 || seenRoles["journal"] == 0 {
		t.Errorf("cells compared %d, by file %v: every file kind must be covered", compared, seenRoles)
	}

	t.Run("TestWALRecordRangeIsInWALArtifact", func(t *testing.T) {
		db, wal, _, _, err := readFixture(fixtureDir, "wal-uncheckpointed")
		if err != nil {
			t.Fatal(err)
		}
		d := openBytes(t, db, wal, nil, bigBudget())
		tb, err := d.Table(t.Context(), "kv", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		inWAL := 0
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			rng, role := r.Range()
			if role == parse.RoleWAL {
				inWAL++
				if rng.Offset < 0 || rng.Length <= 0 || rng.Offset+rng.Length > int64(len(wal)) {
					t.Errorf("WAL range %d+%d is outside the %d-byte WAL artifact", rng.Offset, rng.Length, len(wal))
				}
			}
		})
		if inWAL == 0 {
			t.Error("no row of wal-uncheckpointed has the WAL role")
		}
	})
}

// liveOutcome is the expected result of opening a fixture's files and reading
// its live rows. Fixtures not listed must open and equal the oracle's live
// section.
var liveOutcome = map[string]func(t *testing.T, db, wal, journal []byte, exp *fixtureExpect){
	// Random bytes behind a valid-looking size: not a database, and said to look
	// encrypted. No rows, a typed error.
	"encrypted-like": func(t *testing.T, db, wal, journal []byte, exp *fixtureExpect) {
		if !exp.NotSQLite {
			t.Error("the oracle does not call the fixture a non-database")
		}
		_, err := sqlitedb.Open(t.Context(), filesOf(db, wal, journal), bigBudget())
		if !errors.Is(err, sqlitedb.ErrNotSQLite) || !errors.Is(err, sqlitedb.ErrEncrypted) {
			t.Errorf("Open = %v, want ErrNotSQLite and ErrEncrypted", err)
		}
	},
	// A persistent journal that is not hot (zeroed header): the database is read
	// as stored, the journal is reported and not applied.
	"persist-journal": func(t *testing.T, db, wal, journal []byte, exp *fixtureExpect) {
		d := openBytes(t, db, wal, journal, bigBudget())
		if j := d.Info().Journal; j == nil || j.Hot || j.Applied {
			t.Errorf("Info().Journal = %+v, want a journal that is neither hot nor applied", j)
		}
		compareWithOracle(t, exp, d)
	},
}

// TestLiveRowsEqualFixtureOracleLive: the layer's rows equal the oracle's live
// section for every committed fixture, with every companion file supplied.
func TestLiveRowsEqualFixtureOracleLive(t *testing.T) {
	compared := 0
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			db, wal, journal, exp, err := readFixture(fixtureDir, name)
			if err != nil {
				t.Fatal(err)
			}
			if f, ok := liveOutcome[name]; ok {
				f(t, db, wal, journal, exp)
				return
			}
			d := openBytes(t, db, wal, journal, bigBudget())
			compareWithOracle(t, exp, d)
			compared++
		})
	}
	if compared < 15 {
		t.Errorf("only %d fixtures compared with the oracle's live section", compared)
	}
}
