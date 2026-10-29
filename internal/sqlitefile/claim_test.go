package sqlitefile_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func foldClaimed(names ...string) func(string) bool {
	return func(t string) bool {
		for _, n := range names {
			if strings.EqualFold(n, t) {
				return true
			}
		}
		return false
	}
}

func freelistFixture(t *testing.T) (*sqlitefile.Schema, []sqlitefile.RecoveredRow) {
	t.Helper()
	f := loadFx(t, fixtureSpec{Name: "sqlite3-freelist", Class: "C"})
	d := f.lib(t)
	sch, err := d.Live().Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	rows, _ := collectRows(t, h)
	return sch, rows
}

func txt(s string) sqlitefile.Value {
	return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte(s), Len: int64(len(s)), Enc: sqlitefile.EncUTF8}
}

func num(n int64) sqlitefile.Value { return sqlitefile.Value{Kind: sqlitefile.KindInt, Int: n} }

// peopleValues is a record that strictly fits people(id integer primary key,
// name text, age integer, email text not null): the rowid alias is stored NULL.
func peopleValues() []sqlitefile.Value {
	return []sqlitefile.Value{{}, txt("n"), num(30), txt("e@x.test")}
}

func TestClaimMatch(t *testing.T) {
	sch, rows := freelistFixture(t)
	claimed := foldClaimed("people")
	var fit, guess, none, idx int
	for _, r := range rows {
		got := sch.ClaimMatch(r, claimed)
		switch {
		case r.Index != "":
			idx++
			if got != sqlitefile.ClaimIndexEntry {
				t.Errorf("index entry %q: %s", r.Index, got)
			}
		case strings.EqualFold(r.Table, "people") && r.TableBasis == sqlitefile.BasisFit:
			fit++
			if got != sqlitefile.ClaimYes {
				t.Errorf("fit row of people: %s", got)
			}
		case strings.EqualFold(r.Table, "people") && r.TableBasis == sqlitefile.BasisGuess:
			guess++
			if got != sqlitefile.ClaimBasis {
				t.Errorf("guess row of people: %s", got)
			}
		case r.Table == "":
			none++
			if got != sqlitefile.ClaimBasis && got != sqlitefile.ClaimNotClaimed {
				t.Errorf("row without a table: %s", got)
			}
		case strings.EqualFold(r.Table, "people"):
			// a schema-basis row of the live people table: claimed only when it fits (checked below)
		default:
			if got != sqlitefile.ClaimNotClaimed {
				t.Errorf("row of %q: %s", r.Table, got)
			}
		}
		if g := sch.ClaimMatch(r, foldClaimed()); r.Index == "" && g != sqlitefile.ClaimNotClaimed {
			t.Errorf("nothing claimed, table row of %q: %s", r.Table, g)
		}
	}
	if fit != 12 || guess != 12 {
		t.Errorf("%d fit and %d guess rows labelled people, want 12 and 12 (index entries %d, no table %d)", fit, guess, idx, none)
	}
	// the fixture holds no index entry; a hand-built one is never a table row
	ie := sqlitefile.RecoveredRow{Index: "ix_people", Table: "people", TableBasis: sqlitefile.BasisSchema, Method: sqlitefile.MethodFreelist, Values: peopleValues()}
	if got := sch.ClaimMatch(ie, claimed); got != sqlitefile.ClaimIndexEntry {
		t.Errorf("index entry: %s", got)
	}
	// the claim list compares ASCII case-insensitively, in both directions
	r := sqlitefile.RecoveredRow{Table: "PEOPLE", TableBasis: sqlitefile.BasisSchema, Method: sqlitefile.MethodFreelist, Values: peopleValues()}
	if got := sch.ClaimMatch(r, foldClaimed("people")); got != sqlitefile.ClaimYes {
		t.Errorf("upper-case label, lower-case claim: %s", got)
	}
	r.Table = "people"
	if got := sch.ClaimMatch(r, foldClaimed("PEOPLE")); got != sqlitefile.ClaimYes {
		t.Errorf("lower-case label, upper-case claim: %s", got)
	}
}

func TestClaimMatchRefusesRowNotFittingLiveDefinition(t *testing.T) {
	sch, _ := freelistFixture(t)
	claimed := foldClaimed("people")
	bad := map[string][]sqlitefile.Value{
		"text in the integer primary key position": {txt("7"), txt("n"), num(30), txt("e@x.test")},
		"too many values":                          append(peopleValues(), num(1)),
		"NOT NULL column stored NULL":              {{}, txt("n"), num(30), {}},
		"too few values (email is NOT NULL)":       {{}, txt("n"), num(30)},
		"no values":                                nil,
	}
	for name, vals := range bad {
		r := sqlitefile.RecoveredRow{Table: "people", TableBasis: sqlitefile.BasisSchema, Method: sqlitefile.MethodWALPrior, Values: vals}
		if got := sch.ClaimMatch(r, claimed); got != sqlitefile.ClaimNoLiveFit {
			t.Errorf("%s: %s, want %s", name, got, sqlitefile.ClaimNoLiveFit)
		}
	}
	r := sqlitefile.RecoveredRow{Table: "people", TableBasis: sqlitefile.BasisSchema, Method: sqlitefile.MethodWALPrior, Values: peopleValues()}
	if got := sch.ClaimMatch(r, claimed); got != sqlitefile.ClaimYes {
		t.Errorf("a row that fits: %s", got)
	}
	r.Table = "dropped_long_ago" // labelled with a table the live schema no longer holds
	if got := sch.ClaimMatch(r, foldClaimed("dropped_long_ago")); got != sqlitefile.ClaimNoLiveFit {
		t.Errorf("a table the live schema lacks: %s", got)
	}
}

func TestClaimMatchFitLimitIsNotAClaim(t *testing.T) {
	f := loadFx(t, fixtureSpec{Name: "sqlite3-freelist", Class: "C"})
	d, err := sqlitefile.Open(bytes.NewReader(f.db), int64(len(f.db)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxFitSteps: 1}})
	if err != nil {
		t.Fatal(err)
	}
	sch, err := d.Live().Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := sqlitefile.RecoveredRow{Table: "people", TableBasis: sqlitefile.BasisFit, Method: sqlitefile.MethodFreelist, Values: peopleValues()}
	if got := sch.ClaimMatch(r, foldClaimed("people")); got != sqlitefile.ClaimFitLimit {
		t.Errorf("a fit that hits the step cap: %s, want %s", got, sqlitefile.ClaimFitLimit)
	}
}

// A slack-carved row is never claimed, whatever its basis (4G-R22).
func TestClaimMatchSlackRowIsNotClaimed(t *testing.T) {
	sch, _ := freelistFixture(t)
	for _, basis := range []sqlitefile.TableBasis{sqlitefile.BasisSchema, sqlitefile.BasisFit} {
		byMethod := sqlitefile.RecoveredRow{Table: "people", TableBasis: basis, Method: sqlitefile.MethodPageSlack, Values: peopleValues()}
		byRegion := sqlitefile.RecoveredRow{Table: "people", TableBasis: basis, Method: sqlitefile.MethodFreelist, SlackRegion: "gap", Values: peopleValues()}
		for _, r := range []sqlitefile.RecoveredRow{byMethod, byRegion} {
			if got := sch.ClaimMatch(r, foldClaimed("people")); got != sqlitefile.ClaimNotClaimed {
				t.Errorf("basis %v, method %s, region %q: %s", basis, r.Method, r.SlackRegion, got)
			}
		}
	}
}

// ClaimMatch reads the definition of the table the row is labelled with and no
// other (4G-R24): with 2,000 tables in the schema one call costs at most the
// columns of that table plus one step.
func TestClaimMatchChecksOnlyTheLabelledTable(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	for i := range 2000 {
		b.CreateTable(fmt.Sprintf("t%d", i), fmt.Sprintf("create table t%d(a, b, c)", i))
	}
	db := b.Bytes()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sch, err := d.Live().Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(sch.Objects); n < 2000 {
		t.Fatalf("%d schema objects", n)
	}
	r := sqlitefile.RecoveredRow{Table: "t1999", TableBasis: sqlitefile.BasisFit, Method: sqlitefile.MethodFreelist, Values: []sqlitefile.Value{num(1), num(2), num(3)}}
	got, steps := sqlitefile.ClaimMatchSteps(sch, r, foldClaimed("t1999"))
	if got != sqlitefile.ClaimYes {
		t.Fatalf("verdict %s", got)
	}
	if steps < 1 || steps > 3+1 {
		t.Errorf("one call took %d steps, want 1..%d (columns + 1)", steps, 3+1)
	}
}
