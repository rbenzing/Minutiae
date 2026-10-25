package dbrecover

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// typedRecorder is the RecoveryEmitter of a typed pass run by hand: it keeps the facts of every
// record and of every unmapped row.
type typedRecorder struct {
	typed, unmapped []parse.RecoveredFacts
}

func (e *typedRecorder) Emit(context.Context, records.Record) error {
	panic("a typed pass must never emit a live record")
}

func (e *typedRecorder) EmitRecovered(_ context.Context, _ records.Record, f parse.RecoveredFacts) error {
	e.typed = append(e.typed, f)
	return nil
}

func (e *typedRecorder) Unmapped(_ context.Context, f parse.RecoveredFacts, _ string) error {
	e.unmapped = append(e.unmapped, f)
	return nil
}
func (e *typedRecorder) Warn(context.Context, string, string) error { return nil }
func (e *typedRecorder) Note(string, string)                        {}
func (e *typedRecorder) Progress(int64, int64)                      {}

var _ parse.RecoveryEmitter = (*typedRecorder)(nil)

// Every recovered row ends as a typed record or a db_row, exactly once: the typed pass takes the claimed
// rows that fit the live table, the generic run excluding that claim writes every other row (rows of
// other tables, rows the library could not label, rows that only guess a table, page slack) plus the
// claimed rows the typed pass could not map, which it names by LocatorDigest of their locator.
func TestTypedPassAndGenericRunCoverEveryRowExactlyOnce(t *testing.T) {
	// the plain run: every row the library finds, as db_row
	pc, pin := fixtureCase(t, "sqlite3-freelist")
	mustRun(t, pc, pin, Options{})
	plain := map[string]int{}
	basisNone, tableless := 0, 0
	for _, f := range storedRecords(t, pc) {
		if f.Locator == "" {
			continue
		}
		plain[f.Locator]++
		p := payloadOf(t, f)
		if p["table_basis"] == "none" {
			basisNone++
		}
		if tbl, _ := p["table"].(string); tbl == "" {
			tableless++
		}
	}
	if len(plain) == 0 || basisNone == 0 || tableless == 0 {
		t.Fatalf("scenario: %d rows, %d at basis none, %d without a table: the fixture must hold library-demoted rows", len(plain), basisNone, tableless)
	}

	// the typed pass, by hand, over the same bytes: two claimed rows cannot be mapped
	db, ok := gunzipFile(t, filepath.Join("..", "sqlitefile", "testdata", "sqlite3-freelist.db.gz"))
	if !ok {
		t.Fatal("fixture missing")
	}
	art := parse.Artifact{ID: "A-DB", Size: int64(len(db)), R: bytes.NewReader(db), Platform: parse.PlatformAndroid}
	in := &parse.Input{
		Primary: art, Artifacts: map[string]parse.Artifact{"db": art},
		Limits: parse.DefaultLimits(), Budget: parse.NewBudget(1 << 30).View(),
	}
	m := parse.TableMapping{Table: "people", Map: func(_ parse.MapContext, row parse.Row) ([]records.Record, error) {
		if id, _ := row.Rowid(); id == 103 || id == 109 {
			return nil, rowrun.Unmappable("no usable name")
		}
		return []records.Record{{Type: "test"}}, nil
	}}
	rec := &typedRecorder{}
	if _, err := recrun.Run(t.Context(), in, rec, []parse.TableMapping{m}, []parse.TableClaim{{Role: "db", Table: "people"}}); err != nil {
		t.Fatal(err)
	}
	if len(rec.typed) != 10 || len(rec.unmapped) != 2 {
		t.Fatalf("typed pass: %d records, %d unmapped rows, want 10 and 2", len(rec.typed), len(rec.unmapped))
	}

	// the generic run excluding the claim, keeping the rows the typed pass could not map
	lib := libraryLocators(t, db)
	var keep [][16]byte
	for _, f := range rec.unmapped {
		if !lib[f.Locator] {
			t.Fatalf("the facts name %s, which is not the RecoveredLocator of any library row", f.Locator)
		}
		keep = append(keep, LocatorDigest(f.Locator))
	}
	gc, gin := fixtureCase(t, "sqlite3-freelist")
	res := mustRun(t, gc, gin, Options{ExcludeTables: typedPeople, KeepClaimed: keep})
	if res.Counts.KeptUnmappedClaimed != 2 || res.Counts.SkippedByReason["excluded-by-claim"] != 10 || res.Ingest.Outcome != "complete" {
		t.Fatalf("kept %d, excluded %d, outcome %q, want 2, 10, complete", res.Counts.KeptUnmappedClaimed, res.Counts.SkippedByReason["excluded-by-claim"], res.Ingest.Outcome)
	}
	generic := map[string]int{}
	gNone := 0
	for _, f := range storedRecords(t, gc) {
		if f.Locator == "" {
			continue
		}
		generic[f.Locator]++
		if payloadOf(t, f)["table_basis"] == "none" {
			gNone++
		}
	}
	if gNone != basisNone {
		t.Errorf("%d rows at basis none written, want all %d: an unlabelled row is never claimed", gNone, basisNone)
	}
	typed := map[string]int{}
	for _, f := range rec.typed {
		typed[f.Locator]++
	}
	for loc, n := range typed {
		if generic[loc] != 0 {
			t.Errorf("%s is typed and also a db_row", loc)
		}
		if plain[loc] != n {
			t.Errorf("%s typed %d times, the plain run holds it %d times", loc, n, plain[loc])
		}
	}
	for _, f := range rec.unmapped {
		if generic[f.Locator] != plain[f.Locator] || plain[f.Locator] == 0 {
			t.Errorf("unmapped %s: %d db_rows, want the plain run's %d", f.Locator, generic[f.Locator], plain[f.Locator])
		}
	}
	// union: typed + generic == plain, row for row
	union := map[string]int{}
	for l, n := range generic {
		union[l] += n
	}
	for l, n := range typed {
		union[l] += n
	}
	if len(union) != len(plain) {
		t.Errorf("%d locators covered, the plain run has %d", len(union), len(plain))
	}
	for l, n := range plain {
		if union[l] != n {
			t.Errorf("%s: covered %d times, want %d", l, union[l], n)
		}
	}
}

// libraryLocators is the RecoveredLocator of every row the library finds in db: what the digests a
// composition passes are made of.
func libraryLocators(t *testing.T, db []byte) map[string]bool {
	t.Helper()
	lib, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := lib.History()
	defer h.Release()
	out := map[string]bool{}
	if _, err := h.Rows(t.Context(), func(rr sqlitefile.RecoveredRow) bool {
		out[sqlitefile.RecoveredLocator(rr)] = true
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
