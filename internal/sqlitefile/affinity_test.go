package sqlitefile_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// affinityCases is the plan's table: declared type -> affinity by the five
// rules in order (INT; CHAR, CLOB, TEXT; BLOB or empty; REAL, FLOA, the start of
// DOUBLE; else NUMERIC).
var affinityCases = []struct {
	decl string
	want sqlitefile.Affinity
}{
	{"INT", sqlitefile.AffInteger},
	{"INTEGER", sqlitefile.AffInteger},
	{"TINYINT", sqlitefile.AffInteger},
	{"BIGINT", sqlitefile.AffInteger},
	{"UNSIGNED BIG INT", sqlitefile.AffInteger},
	{"VARCHAR(255)", sqlitefile.AffText},
	{"NCHAR(55)", sqlitefile.AffText},
	{"CLOB", sqlitefile.AffText},
	{"TEXT", sqlitefile.AffText},
	{"BLOB", sqlitefile.AffBlob},
	{"", sqlitefile.AffBlob},
	{"REAL", sqlitefile.AffReal},
	{"DOUBLE", sqlitefile.AffReal},
	{"DOUBLE PRECISION", sqlitefile.AffReal},
	{"FLOAT", sqlitefile.AffReal},
	{"FLOATING POINT", sqlitefile.AffInteger}, // "FLOATING POINT" contains INT
	{"NUMERIC", sqlitefile.AffNumeric},
	{"DECIMAL(10,5)", sqlitefile.AffNumeric},
	{"BOOLEAN", sqlitefile.AffNumeric},
	{"DATE", sqlitefile.AffNumeric},
	{"STRING", sqlitefile.AffNumeric},
	{"POINT", sqlitefile.AffInteger}, // POINT contains INT
	// Rule order and case: the first matching rule wins, matching is case-blind.
	{"int", sqlitefile.AffInteger},
	{"Varchar(10)", sqlitefile.AffText},
	{"CHARINT", sqlitefile.AffInteger},
	{"TEXTBLOB", sqlitefile.AffText},
	{"BLOBREAL", sqlitefile.AffBlob},
	{"CLOBFLOAT", sqlitefile.AffText},
	{"FLOA", sqlitefile.AffReal},
	{"DOUB", sqlitefile.AffReal}, //nolint:misspell // the engine's rule keys on this prefix
	{"REA", sqlitefile.AffNumeric},
	{"ANY", sqlitefile.AffNumeric},
	{"é", sqlitefile.AffNumeric},
	{"ÉINT", sqlitefile.AffInteger},
}

func TestAffinityRules(t *testing.T) {
	for _, c := range affinityCases {
		if got := sqlitefile.AffinityOf(c.decl); got != c.want {
			t.Errorf("AffinityOf(%q) = %d, want %d", c.decl, got, c.want)
		}
	}
}

// engineAffinity classifies a declared type by what the engine does with it:
// a column of that type given text '1', '1.5' and the integer 7, and CAST of
// the text to that type.
func engineAffinity(t *testing.T, decl string) sqlitefile.Affinity {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "aff.db")) // a fresh database per type
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(fmt.Sprintf("create table probe(x %s)", decl)); err != nil {
		t.Fatalf("engine rejects type %q: %v", decl, err)
	}
	q := func(sqltext string, args ...any) string {
		var s string
		if err := db.QueryRow(sqltext, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sqltext, err)
		}
		return s
	}
	const ins = "insert into probe(x) values(?)"
	for _, v := range []any{"1", "1.5", int64(7)} {
		if _, err := db.Exec(ins, v); err != nil {
			t.Fatal(err)
		}
	}
	got := q("select group_concat(typeof(x), ',') from (select x from probe order by rowid)")
	switch got {
	case "text,text,text":
		return sqlitefile.AffText
	case "text,text,integer":
		return sqlitefile.AffBlob
	case "real,real,real":
		return sqlitefile.AffReal
	case "integer,real,integer":
		// INTEGER and NUMERIC differ only in CAST.
		c := q(fmt.Sprintf("select typeof(cast('1.5' as %s))", decl))
		if c == "integer" {
			return sqlitefile.AffInteger
		}
		return sqlitefile.AffNumeric
	}
	t.Fatalf("type %q: unexpected engine behaviour %q", decl, got)
	return 0
}

// TestAffinityMatchesEngine pins the five rules against the engine: every
// declared type of the table (except the one the engine cannot declare) gets
// the affinity the engine gives a column of that type.
func TestAffinityMatchesEngine(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range affinityCases {
		d := strings.TrimSpace(c.decl)
		if seen[d] || strings.ContainsAny(d, "é") {
			continue
		}
		seen[d] = true
		if got, want := sqlitefile.AffinityOf(d), engineAffinity(t, d); got != want {
			t.Errorf("AffinityOf(%q) = %d, the engine treats a column of that type as %d", d, got, want)
		}
	}
	if sqlitefile.AffinityOf("") != sqlitefile.AffBlob {
		t.Error("an empty type has the BLOB (none) affinity")
	}
}
