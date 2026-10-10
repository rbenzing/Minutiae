package rowrun_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
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
