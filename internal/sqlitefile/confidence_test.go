package sqlitefile_test

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// TestConfidenceRule is the golden table of spec 3 section 3.3 with the D2
// basis caps: one row per method, one column per basis (schema, fit, guess,
// none). Every number is written out, none computed.
func TestConfidenceRule(t *testing.T) {
	bases := []sqlitefile.TableBasis{sqlitefile.BasisSchema, sqlitefile.BasisFit, sqlitefile.BasisGuess, sqlitefile.BasisNone}
	golden := []struct {
		method string
		want   [4]int
	}{
		{sqlitefile.MethodWALPrior, [4]int{75, 60, 40, 30}},
		{sqlitefile.MethodJournalBefore, [4]int{70, 60, 40, 30}},
		{sqlitefile.MethodFreelist, [4]int{60, 60, 40, 30}},
		{sqlitefile.MethodWALStale, [4]int{55, 55, 40, 30}},
		{sqlitefile.MethodWALUncommitted, [4]int{45, 45, 40, 30}},
		{sqlitefile.MethodJournalRolledBack, [4]int{45, 45, 40, 30}},
		{sqlitefile.MethodJournalPersist, [4]int{35, 35, 35, 30}},
		{sqlitefile.MethodPageSlack, [4]int{25, 25, 25, 25}},
	}
	for _, g := range golden {
		for i, b := range bases {
			got := sqlitefile.Confidence(sqlitefile.RecoveredRow{Method: g.method, TableBasis: b})
			if got != g.want[i] {
				t.Errorf("%s at %s: %d, want %d", g.method, b, got, g.want[i])
			}
		}
	}
	// a basis the rule does not know is the lowest cap, never a stronger one
	if got := sqlitefile.Confidence(sqlitefile.RecoveredRow{Method: sqlitefile.MethodWALPrior, TableBasis: "other"}); got != 30 {
		t.Errorf("unknown basis: %d, want 30", got)
	}
	if got := sqlitefile.Confidence(sqlitefile.RecoveredRow{Method: sqlitefile.MethodWALPrior}); got != 30 {
		t.Errorf("empty basis: %d, want 30", got)
	}
	for c, want := range map[int]string{-1: "low", 0: "low", 39: "low", 40: "medium", 69: "medium", 70: "high", 100: "high"} {
		if got := sqlitefile.Band(c); got != want {
			t.Errorf("Band(%d) = %q, want %q", c, got, want)
		}
	}
}

func TestConfidenceCap(t *testing.T) {
	ten, eighty := 10, 80
	for _, c := range []struct {
		c    int
		cap  *int
		want int
	}{{55, nil, 55}, {55, &ten, 10}, {55, &eighty, 55}, {55, new(int), 0}, {0, &eighty, 0}} {
		if got := sqlitefile.CapConfidence(c.c, c.cap); got != c.want {
			t.Errorf("CapConfidence(%d, %v) = %d, want %d", c.c, c.cap, got, c.want)
		}
	}
}

func TestConfidenceUnknownMethodIsZero(t *testing.T) {
	for _, m := range []string{"", "sqlite-unknown", "SQLITE-WAL-PRIOR", "carve-validated"} {
		for _, b := range []sqlitefile.TableBasis{sqlitefile.BasisSchema, sqlitefile.BasisFit, sqlitefile.BasisNone} {
			if got := sqlitefile.Confidence(sqlitefile.RecoveredRow{Method: m, TableBasis: b}); got != 0 {
				t.Errorf("method %q basis %s: %d, want 0", m, b, got)
			}
		}
	}
}

// TestMethodTokensSyntax pins the token pattern. The same literal defines
// records.Record.Recovery in internal/records/model.go; plan 3J adds
// TestMethodTokensAcceptedByRecords where both packages may be imported.
func TestMethodTokensSyntax(t *testing.T) {
	re := regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	want := []string{
		"sqlite-wal-prior", "sqlite-journal-before", "sqlite-wal-stale", "sqlite-freelist",
		"sqlite-wal-uncommitted", "sqlite-journal-rolledback", "sqlite-journal-persist", "sqlite-page-slack",
	}
	got := []string{
		sqlitefile.MethodWALPrior, sqlitefile.MethodJournalBefore, sqlitefile.MethodWALStale, sqlitefile.MethodFreelist,
		sqlitefile.MethodWALUncommitted, sqlitefile.MethodJournalRolledBack, sqlitefile.MethodJournalPersist, sqlitefile.MethodPageSlack,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokens %q, want %q", got, want)
	}
	for _, m := range got {
		if !re.MatchString(m) {
			t.Errorf("token %q does not match %s", m, re)
		}
	}
}

// TestConfidenceIsPure: the same row twice gives the same rank, and the values,
// location and notes of a row never change it.
func TestConfidenceIsPure(t *testing.T) {
	base := sqlitefile.RecoveredRow{Method: sqlitefile.MethodFreelist, TableBasis: sqlitefile.BasisFit}
	want := sqlitefile.Confidence(base)
	if want != 60 || sqlitefile.Confidence(base) != want {
		t.Fatalf("confidence %d, want 60 both times", want)
	}
	rid := int64(7)
	other := base
	other.Values = []sqlitefile.Value{{Kind: sqlitefile.KindText, Bytes: []byte("secret"), Len: 6}}
	other.Rowid, other.Table, other.Truncated, other.OverflowHead = &rid, "t", true, 9
	other.Notes = []string{"x"}
	other.Relation = sqlitefile.RelAbsentFromLive
	other.Loc = sqlitefile.Loc{File: sqlitefile.FileWAL, Page: 3}
	other.WAL = &sqlitefile.WALProv{Frame: 4}
	if got := sqlitefile.Confidence(other); got != want {
		t.Errorf("confidence %d depends on the row content, want %d", got, want)
	}
}
