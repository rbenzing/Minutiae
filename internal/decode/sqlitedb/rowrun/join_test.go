package rowrun_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

func TestMatchOnACleanTableIsDecided(t *testing.T) {
	mc := openCtx(t, tinyDB(t, 5)) // t.a = 10 * id
	rows, undecided, err := rowrun.Match(mc, "t", "a", 30)
	if err != nil || undecided || len(rows) != 1 {
		t.Fatalf("hit: %d rows, undecided %v, %v", len(rows), undecided, err)
	}
	if id, _ := rows[0].Rowid(); id != 3 {
		t.Errorf("hit is rowid %d, want 3", id)
	}
	rows, undecided, err = rowrun.Match(mc, "t", "a", 31)
	if err != nil || undecided || len(rows) != 0 {
		t.Errorf("miss on a clean table: %d rows, undecided %v, %v; want a decided empty answer", len(rows), undecided, err)
	}
}

// Review Focus 1: on a table that lost rows no answer is trusted, neither a miss nor a hit.
func TestMatchFlagsEveryAnswerOfALossyIndex(t *testing.T) {
	mc := openCtx(t, damagedDB(t)) // t.a = rowid, the rows of the second leaf can never be delivered
	hits, misses := 0, 0
	for id := int64(1); id <= 300; id++ {
		rows, undecided, err := rowrun.Match(mc, "t", "a", id)
		if err != nil {
			t.Fatalf("Match(%d): %v", id, err)
		}
		if !undecided {
			t.Fatalf("Match(%d) = %d rows, decided, on a table that lost rows", id, len(rows))
		}
		if len(rows) == 1 {
			hits++
		} else if len(rows) == 0 {
			misses++
		}
	}
	if hits == 0 || misses == 0 {
		t.Errorf("hits %d misses %d: the damage should lose some rows and keep others", hits, misses)
	}
}

func TestMatchErrorsAndForeignContexts(t *testing.T) {
	mc := openCtx(t, tinyDB(t, 2))
	if _, _, err := rowrun.Match(mc, "nope", "a", 1); !errors.Is(err, sqlitedb.ErrNoSuchTable) {
		t.Errorf("missing table: %v, want ErrNoSuchTable", err)
	}
	if _, _, err := rowrun.Match(mc, "t", "nope", 1); !errors.Is(err, sqlitedb.ErrUnsupportedSchema) {
		t.Errorf("missing column: %v, want ErrUnsupportedSchema", err)
	}
	rows, undecided, err := rowrun.Match(fakeMC{}, "t", "a", 1)
	if rows != nil || undecided || err != nil {
		t.Errorf("a context without Joins: %v %v %v", rows, undecided, err)
	}
}

// T37: ErrKeyUndecidable on a clean table, reached through a key the index cannot compare.
func TestMatchReportsAnUndecidableKeyOnACleanTable(t *testing.T) {
	mc := openCtx(t, tinyDB(t, 3))
	rows, undecided, err := rowrun.MatchKeyForTest(mc, "t", "a", parse.JoinKey{Kind: parse.JoinKeyKind(200)})
	if err != nil || !undecided || rows != nil {
		t.Fatalf("rows %v undecided %v err %v; want nil rows, undecided, no error", rows, undecided, err)
	}
	var found bool
	for _, j := range mc.Joins() {
		if j.Table == "t" && j.Column == "a" {
			found = true
			if j.Undecidable != 1 {
				t.Errorf("Undecidable = %d, want 1", j.Undecidable)
			}
		}
	}
	if !found {
		t.Error("the undecidable lookup was not counted in the join report")
	}
}

// T36: a caller bug is a typed error, never a panic.
func TestMatchRefusesANilMatcher(t *testing.T) {
	if _, _, err := rowrun.Match(nil, "t", "a", 1); !errors.Is(err, rowrun.ErrNilInput) {
		t.Errorf("nil matcher: %v, want ErrNilInput", err)
	}
}

// T35: the single-join accessor reads one join's counter without copying the others.
func TestUndecidedHitsReadsOneJoin(t *testing.T) {
	mc := openCtx(t, damagedDB(t))
	if n := mc.UndecidedHits("t", "a"); n != 0 {
		t.Fatalf("before any lookup: %d", n)
	}
	for id := int64(1); id <= 300; id++ {
		if _, _, err := rowrun.Match(mc, "t", "a", id); err != nil {
			t.Fatal(err)
		}
	}
	var want int64
	for _, j := range mc.Joins() {
		if j.Table == "t" && j.Column == "a" {
			want = j.UndecidedHits
		}
	}
	if want == 0 || mc.UndecidedHits("T", "A") != want {
		t.Errorf("UndecidedHits = %d (case-folded), Joins says %d", mc.UndecidedHits("T", "A"), want)
	}
	if mc.UndecidedHits("t", "nope") != 0 {
		t.Error("an unknown join must read 0")
	}
}
