package sqlitedb_test

// Probe-side affinity conversion (LX-4 Task 5, L122 d part 1): a join key whose own
// affinity is weaker than the target column's is converted like a constant, exactly
// or not at all. The engine is the oracle for every case.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
)

// enginePairsBySource groups the engine's pairs by source rowid.
func enginePairsBySource(pairs map[[2]int64]bool) map[int64][]int64 {
	out := map[int64][]int64{}
	for p := range pairs {
		out[p[0]] = append(out[p[0]], p[1])
	}
	for _, ids := range out {
		slices.Sort(ids)
	}
	return out
}

// A source column with no declared type holding 7, 'x' and 7.5 against a TEXT
// column. The plan expected the engine to apply TEXT affinity to the source; the
// engine does not: two columns of which neither has numeric affinity are
// compared as stored (only a constant or parameter takes a column's affinity).
// So 7 does NOT meet '7', 7.5 does not meet '7.5', and nothing is undecidable.
// The layer must convert nothing here: converting the integer to text would
// invent a pair the engine does not make. (Passes on arrival; the mutation that
// converts a ClassBlob key for a TEXT target fails it.)
func TestNoAffinityKeyAgainstATextColumnIsNotConverted(t *testing.T) {
	data := engineTwoTables(t, "UTF-8", "", "text", []any{7, "x", 7.5}, []any{"7", "x", "7.5", "y"})
	engine := enginePairsBySource(enginePairs(t, data))
	if len(engine) != 1 || !slices.Equal(engine[2], []int64{2}) {
		t.Fatalf("the engine pairs %v: the case is meant to join only 'x' with 'x'", engine)
	}
	lookups := layerLookups(t, data)
	if len(lookups) != 3 {
		t.Fatalf("%d lookups, want 3", len(lookups))
	}
	for _, l := range lookups {
		if l.err != nil {
			t.Fatalf("source %d: %v", l.source, l.err)
		}
		if !slices.Equal(l.ids, engine[l.source]) {
			t.Errorf("source %d: ids %v, the engine joins %v", l.source, l.ids, engine[l.source])
		}
	}
}

// The conversion changes the key's value, never the collation it is compared
// under: a TEXT source against an INTEGER target declared COLLATE NOCASE still
// folds the texts that are not numbers by the target's collation. (Passes on
// arrival: the collation was already the target's; the mutation that builds the
// probe key under BINARY after the conversion fails it.)
func TestProbeConversionKeepsTheTargetCollation(t *testing.T) {
	data := engineTwoTables(t, "UTF-8", "text", "integer collate nocase",
		[]any{"ABC", "x", "Q"}, []any{"abc", "Abc", "x", "z"})
	engine := enginePairsBySource(enginePairs(t, data))
	if len(engine[1]) != 2 {
		t.Fatalf("the engine joins %v: 'ABC' is meant to meet 'abc' and 'Abc' under NOCASE", engine)
	}
	for _, l := range layerLookups(t, data) {
		if l.err != nil {
			t.Fatalf("source %d: %v", l.source, l.err)
		}
		if !slices.Equal(l.ids, engine[l.source]) {
			t.Errorf("source %d: ids %v, the engine joins %v", l.source, l.ids, engine[l.source])
		}
	}
}

// A source column of 20000 distinct numeric-looking and hostile texts against an
// INTEGER column: every lookup is engine-equal or ErrKeyUndecidable, nothing
// panics, and the index returns its budget when released.
func TestProbeSideConversionOfAHostileSourceColumn(t *testing.T) {
	const n = 20000
	var svals []any
	for i := range n {
		switch i % 10 {
		case 0:
			svals = append(svals, fmt.Sprintf(" %d ", i%900))
		case 1:
			svals = append(svals, strings.Repeat("9", 4096-5)+fmt.Sprintf("%05d", i))
		case 2:
			svals = append(svals, fmt.Sprintf("%dinf", i))
		case 3:
			svals = append(svals, fmt.Sprintf("%d\x00%d", i%900, i))
		case 4:
			svals = append(svals, fmt.Sprintf("0.%015d", i))
		case 5:
			svals = append(svals, fmt.Sprintf("%de%d", i%900, i%30))
		case 6:
			svals = append(svals, fmt.Sprintf("-%d", i%900))
		case 7:
			svals = append(svals, fmt.Sprintf("0x%x", i%900))
		case 8:
			svals = append(svals, fmt.Sprintf("%d.", i%900))
		default:
			svals = append(svals, fmt.Sprintf("%d.5", i%900))
		}
	}
	svals = append(svals, "inf", "-inf", "nan", "infinity", "1e999", strings.Repeat(" ", 4096)+"7")
	var gvals []any
	for i := range 900 {
		gvals = append(gvals, i, -i)
	}

	p := filepath.Join(t.TempDir(), "w.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("create table s(id integer primary key, k text)")
	exec("create table g(id integer primary key, k integer)")
	exec("begin")
	for i, v := range svals {
		exec("insert into s(id, k) values(?, ?)", i+1, v)
	}
	for i, v := range gvals {
		exec("insert into g(id, k) values(?, ?)", i+1, v)
	}
	exec("commit")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	engine := enginePairsBySource(enginePairs(t, data))
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	src, err := d.Table(t.Context(), "s", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tgt, err := d.Table(t.Context(), "g", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeIndex := b.used
	ix, err := tgt.Index(t.Context(), "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	decided, undecided := 0, 0
	if err := src.Scan(t.Context(), func(row sqlitedb.Row) error {
		sid, _ := row.Rowid()
		k, ok, kerr := sqlitedb.KeyOf(row, 1)
		if kerr != nil || !ok {
			return fmt.Errorf("KeyOf %d: %v %v", sid, ok, kerr)
		}
		ids, _, err := ix.Rowids(k)
		if err != nil {
			if !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
				return fmt.Errorf("source %d: %w", sid, err)
			}
			undecided++
			return nil
		}
		decided++
		if !slices.Equal(ids, engine[sid]) {
			return fmt.Errorf("source %d (%.20q): ids %v, the engine joins %v", sid, svals[sid-1], ids, engine[sid])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if decided == 0 || undecided == 0 {
		t.Errorf("decided %d, undecided %d: the column is meant to hold both", decided, undecided)
	}
	during := b.used
	ix.Release()
	if b.used >= during {
		t.Errorf("Index.Release returned nothing (%d -> %d, %d before the index)", during, b.used, beforeIndex)
	}
	d.Release()
	if b.used != 0 {
		t.Errorf("budget %d after Release of the index and the database, want 0", b.used)
	}
}
