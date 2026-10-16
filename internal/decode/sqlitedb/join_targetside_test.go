package sqlitedb_test

// Target-side affinity conversion (LX-4 Task 6, L122 d part 2): a numeric source
// column against a TEXT or no-affinity target column converts the TARGET's
// numeric-looking texts (the engine applies NUMERIC affinity to the other
// operand). The opposite direction does not exist (U49): a TEXT source against a
// no-affinity target compares as stored, so nothing in the target is rendered as
// text. The engine is the oracle for every case, in both directions.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// checkTargetSide runs the layer against the engine over the tables s and g of
// data. A lookup is either engine-equal or ErrKeyUndecidable where allowUndecided
// says so; the number of decided and undecided lookups is returned.
func checkTargetSide(t *testing.T, data []byte, allowUndecided func(source int64) bool) (decided, undecided int) {
	t.Helper()
	engine := enginePairsBySource(enginePairs(t, data))
	for _, l := range layerLookups(t, data) {
		if l.err != nil {
			if !errors.Is(l.err, sqlitedb.ErrKeyUndecidable) || allowUndecided == nil || !allowUndecided(l.source) {
				t.Errorf("source %d: %v", l.source, l.err)
			}
			undecided++
			continue
		}
		decided++
		if !slices.Equal(l.ids, engine[l.source]) {
			t.Errorf("source %d: ids %v, the engine joins %v", l.source, l.ids, engine[l.source])
		}
	}
	return decided, undecided
}

// The engine converts the TARGET text to a number when the source is numeric:
// 7 meets '7', '07', ' 7', '7.0', '+7' and '7 '; 1 meets '1e0'; '0x7', 'abc' and ”
// are not numbers.
func TestNumericKeyAgainstATextColumnMatchesTheEngine(t *testing.T) {
	gvals := []any{"7", "07", " 7", "7.0", "1e0", "abc", "", "0x7", "1", "2.5", "+7", "-0", "7 ", "7abc"}
	for _, gdecl := range []string{"text", "text collate nocase", "text collate rtrim"} {
		for _, tc := range []struct {
			name, sdecl string
			svals       []any
		}{
			{"integer", "integer", []any{7, 1, 0, -3, 9}},
			{"real", "real", []any{7.0, 1.0, 2.5, 0.0, 3.25}},
			{"numeric", "numeric", []any{7, 2.5, 1, "abc", 8}},
		} {
			t.Run(gdecl+"/"+tc.name, func(t *testing.T) {
				data := engineTwoTables(t, "UTF-8", tc.sdecl, gdecl, tc.svals, gvals)
				if n := len(enginePairs(t, data)); n < 5 {
					t.Fatalf("the engine joins %d pairs; the case is meant to hold many", n)
				}
				checkTargetSide(t, data, nil)
			})
		}
	}
	t.Run("utf16", func(t *testing.T) {
		data := engineTwoTables(t, "UTF-16le", "integer", "text", []any{7, 1, 5}, gvals)
		if n := len(enginePairs(t, data)); n < 5 {
			t.Fatalf("the engine joins %d pairs; the case is meant to hold many", n)
		}
		checkTargetSide(t, data, nil)
	})
}

func TestNumericKeyAgainstANoAffinityColumnMatchesTheEngine(t *testing.T) {
	gvals := []any{7, "7", 7.0, "abc", []byte{0x37}, "07", 7.5, "7.5", []byte("7")}
	for _, sdecl := range []string{"integer", "real", "numeric"} {
		t.Run(sdecl, func(t *testing.T) {
			data := engineTwoTables(t, "UTF-8", sdecl, "", []any{7, 7.5, 1, 9}, gvals)
			if n := len(enginePairs(t, data)); n < 5 {
				t.Fatalf("the engine joins %d pairs; the case is meant to hold many", n)
			}
			checkTargetSide(t, data, nil)
		})
	}
	t.Run("blob-nocase", func(t *testing.T) {
		data := engineTwoTables(t, "UTF-8", "integer", "blob collate nocase", []any{7, 1}, gvals)
		checkTargetSide(t, data, nil)
	})
}

// U49, the dropped direction: a TEXT source against a no-affinity target is
// compared as stored. The target's 7 and 7.5 are NOT rendered as '7' and '7.5':
// the engine joins only text with text, the layer answers the same, and nothing is
// undecidable (no real needs a spelling). The mirror (a no-affinity key against a
// TEXT target) is TestNoAffinityKeyAgainstATextColumnIsNotConverted.
func TestTextKeyAgainstANoAffinityColumnIsNotConverted(t *testing.T) {
	data := engineTwoTables(t, "UTF-8", "text", "", []any{"7", "7.5", "x"}, []any{7, 7.5, "7", "x", []byte("7")})
	engine := enginePairsBySource(enginePairs(t, data))
	if len(engine) != 2 || !slices.Equal(engine[1], []int64{3}) || !slices.Equal(engine[3], []int64{4}) {
		t.Fatalf("the engine pairs %v; the case is meant to join '7' with '7' and 'x' with 'x' only", engine)
	}
	decided, undecided := checkTargetSide(t, data, nil)
	if decided != 3 || undecided != 0 {
		t.Errorf("decided %d, undecided %d, want 3 and 0", decided, undecided)
	}
}

// A target text that looks like a number but cannot be converted exactly makes a
// numeric probe's miss undecidable (the count is named) and its hit flagged.
func TestOverlongNumericTextInTheTargetMakesTheMissUndecidable(t *testing.T) {
	data := engineTwoTables(t, "UTF-8", "integer", "text", []any{7, 5}, []any{"12345678901234567890", "7", "x"})
	lookups := layerLookups(t, data)
	if len(lookups) != 2 {
		t.Fatalf("%d lookups, want 2", len(lookups))
	}
	if l := lookups[0]; l.err != nil || !slices.Equal(l.ids, []int64{2}) || l.flags&sqlitedb.JoinUndecided == 0 {
		t.Errorf("the hit on 7: ids %v, flags %b, err %v; want [2] flagged JoinUndecided", l.ids, l.flags, l.err)
	}
	l := lookups[1]
	if !errors.Is(l.err, sqlitedb.ErrKeyUndecidable) || !strings.Contains(l.err.Error(), "1 value") {
		t.Errorf("the miss on 5: ids %v, err %v; want ErrKeyUndecidable naming 1 value", l.ids, l.err)
	}
}

func textColumnOf(n int, numeric bool) []any {
	var out []any
	for i := range n {
		if numeric {
			out = append(out, fmt.Sprint(i+1))
		} else {
			out = append(out, fmt.Sprintf("name%d", i))
		}
	}
	return out
}

func targetTable(t *testing.T, data []byte, b sqlitedb.Budget) *sqlitedb.Table {
	t.Helper()
	d := openBytes(t, data, nil, nil, b)
	tgt, err := d.Table(t.Context(), "g", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Warm: the reader keeps its pages, which are not the index's memory.
	if err := tgt.Scan(t.Context(), func(sqlitedb.Row) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return tgt
}

// 100 numeric texts keep 100 primary and 100 alternative entries, all counted
// toward the limit: going over it is ErrIndexLimit, never a dropped alternative.
func TestAltEntriesCountTowardTheIndexLimit(t *testing.T) {
	data := engineTwoTables(t, "UTF-8", "integer", "text", []any{1}, textColumnOf(100, true))
	b := bigBudget()
	tgt := targetTable(t, data, b)
	before := b.used
	if ix, err := tgt.Index(t.Context(), "k", 150); !errors.Is(err, sqlitedb.ErrIndexLimit) || ix != nil {
		t.Fatalf("limit 150: %v, %v; want ErrIndexLimit", ix, err)
	}
	if b.used != before {
		t.Errorf("budget %d after the refused build, want %d (nothing kept)", b.used, before)
	}
	ix, err := tgt.Index(t.Context(), "k", 200)
	if err != nil {
		t.Fatalf("limit 200: %v", err)
	}
	if n := ix.AltEntries(); n != 100 {
		t.Errorf("%d alternative entries, want 100", n)
	}
}

func TestAltEntriesAreChargedAndReleased(t *testing.T) {
	num := engineTwoTables(t, "UTF-8", "integer", "text", []any{1}, textColumnOf(100, true))
	plain := engineTwoTables(t, "UTF-8", "integer", "text", []any{1}, textColumnOf(100, false))
	delta := func(data []byte) (grown, peak int64) {
		b := bigBudget()
		tgt := targetTable(t, data, b)
		before := b.used
		ix, err := tgt.Index(t.Context(), "k", 0)
		if err != nil {
			t.Fatal(err)
		}
		grown = b.used - before
		ix.Release()
		if b.used != before {
			t.Errorf("budget %d after Release, want %d", b.used, before)
		}
		return grown, b.peak
	}
	gn, pn := delta(num)
	gp, pp := delta(plain)
	if gn <= gp || pn <= pp {
		t.Errorf("held %d (peak %d) with alternative entries, %d (peak %d) without: the alternatives are not charged", gn, pn, gp, pp)
	}

	// A budget that cannot hold the alternative entries refuses the build with
	// ErrBudget and keeps nothing.
	short := &testBudget{limit: pp + (pn-pp)/2}
	tgt := targetTable(t, num, short)
	before := short.used
	if ix, err := tgt.Index(t.Context(), "k", 0); !errors.Is(err, parse.ErrBudget) || ix != nil {
		t.Fatalf("short budget: %v, %v; want ErrBudget", ix, err)
	}
	if short.used != before {
		t.Errorf("budget %d after the refused build, want %d", short.used, before)
	}
}

// The classifier reads at most 64 bytes of a value after its blanks, so a column of
// hostile texts (blank runs, long digit strings, NULs, spellings of infinity) costs
// bounded work, never panics, and every lookup is a subset of the engine's pairs
// flagged JoinUndecided, or ErrKeyUndecidable.
func TestTargetSideConversionOfAHostileColumn(t *testing.T) {
	const n = 20000
	var gvals []any
	for i := range n {
		switch i % 8 {
		case 0:
			gvals = append(gvals, fmt.Sprintf("%d", i%60))
		case 1:
			gvals = append(gvals, strings.Repeat(" ", 3000)+fmt.Sprintf("%d", i%60))
		case 2:
			gvals = append(gvals, strings.Repeat("9", 4000)+fmt.Sprintf("%05d", i))
		case 3:
			gvals = append(gvals, fmt.Sprintf("%d\x00%d", i%60, i))
		case 4:
			gvals = append(gvals, fmt.Sprintf("%dinf", i))
		case 5:
			gvals = append(gvals, fmt.Sprintf("0.%015d", i))
		case 6:
			gvals = append(gvals, fmt.Sprintf(" %d. ", i%60))
		default:
			gvals = append(gvals, fmt.Sprintf("x%d", i))
		}
	}
	var svals []any
	for i := range 60 {
		svals = append(svals, i, float64(i)+0.5)
	}
	data := engineTwoTables(t, "UTF-8", "numeric", "text", svals, gvals)
	engine := enginePairsBySource(enginePairs(t, data))
	decided, undecided := 0, 0
	for _, l := range layerLookups(t, data) {
		if l.err != nil {
			if !errors.Is(l.err, sqlitedb.ErrKeyUndecidable) {
				t.Fatalf("source %d: %v", l.source, l.err)
			}
			undecided++
			continue
		}
		decided++
		if len(l.ids) > 0 && l.flags&sqlitedb.JoinUndecided == 0 {
			t.Errorf("source %d: a hit on an index with undecided rows without JoinUndecided", l.source)
		}
		for _, id := range l.ids {
			if !slices.Contains(engine[l.source], id) {
				t.Errorf("source %d: id %d is not a pair of the engine", l.source, id)
			}
		}
	}
	if decided == 0 || undecided == 0 {
		t.Errorf("decided %d, undecided %d: the column is meant to give both", decided, undecided)
	}
}

// The alternatives exist only for values that have a cross-class representation
// (PASSES ON ARRIVAL as a structural pin; mutation: build altNum for every
// target and see the numeric-target case fail).
func TestIndexBuildsAlternativeEntriesOnlyForCrossClassValues(t *testing.T) {
	for _, tc := range []struct {
		name, gdecl string
		gvals       []any
		want        int
	}{
		{"numeric-target", "integer", []any{7, 8, "9", 10.5}, 0},
		{"real-target", "real", []any{7, "9"}, 0},
		{"text-all-names", "text", []any{"a", "b", "7x"}, 0},
		{"blob-all-blobs", "", []any{[]byte("7"), []byte{1, 2}}, 0},
		{"empty", "text", nil, 0},
		{"no-affinity-numbers-only", "", []any{7, 7.5}, 0},
		// The cost, stated: one extra entry per numeric-looking value.
		{"text-numeric-looking", "text", []any{"7", "07", "x"}, 2},
		// U49: a no-affinity column holding '7' and the integer 7 gets ONE
		// alternative (the number of the text); the integer is not rendered as text.
		{"no-affinity-text-and-integer", "", []any{"7", 7}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := engineTwoTables(t, "UTF-8", "integer", tc.gdecl, []any{1}, tc.gvals)
			ix, err := targetTable(t, data, bigBudget()).Index(t.Context(), "k", 0)
			if err != nil {
				t.Fatal(err)
			}
			if n := ix.AltEntries(); n != tc.want {
				t.Errorf("%d alternative entries, want %d", n, tc.want)
			}
		})
	}
}
