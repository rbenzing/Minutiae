package records_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// queryDoc is one document of the query fixture: raw summary and body, indexed through
// evidence.NewFTSDoc exactly as the writer does.
type queryDoc struct {
	id            int64
	summary, body string
}

// queryDocs is a small crafted corpus: words that look like FTS5 syntax next to the literal words an
// operator reading of them would hit, so a query that leaks an operator matches a different set.
var queryDocs = []queryDoc{
	{1, "apple pie", "zzz"},
	{2, "a", "x"},
	{3, "x a b", "y"},
	{4, "near a b", ""},
	{5, "col x", ""},
	{6, "summary foo", "foo"},
	{7, "foo bar", "ab"},
	{8, "and", "or not near"},
	{9, "axb a b", ""},
	{10, "x y", `quoted "x" y`},
	{11, "{summary}: literal", ""},
	{12, `back\slash`, ""},
	{13, `NEAR(a b) OR "x" col:y -z a*b`, ""},
	{14, "caf\u00e9 au lait", ""},
	{15, "Stra\u00dfe 12", ""},
	{16, "foo", "bar"},
}

// queryFixture is a real in-memory SQLite database holding both full-text tables (the shared DDL) of
// the query fixture. It has one connection, so concurrent users are serialised.
type queryFixture struct{ db *sql.DB }

func newQueryFixture(tb testing.TB) *queryFixture {
	tb.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		tb.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	for _, table := range evidence.FTSTables() {
		ddl, ok := evidence.FTSTableDDL(table)
		if !ok {
			tb.Fatalf("no DDL for %s", table)
		}
		if _, err := db.Exec(ddl); err != nil {
			tb.Fatalf("%s: %v", ddl, err)
		}
		for _, d := range queryDocs {
			body := d.body
			doc, ok := evidence.NewFTSDoc(d.id, d.summary, &body)
			if !ok {
				continue
			}
			if _, err := db.Exec(`INSERT INTO `+table+`(rowid, summary, body) VALUES (?, ?, ?)`, doc.ID, doc.Summary, doc.Body); err != nil { //nolint:gosec // one of the two FTS table constants
				tb.Fatalf("insert %d into %s: %v", d.id, table, err)
			}
		}
	}
	return &queryFixture{db: db}
}

// match executes the expression the way the search does (bound as ?) and returns the matching ids.
func (f *queryFixture) match(table, expr string) ([]int64, error) {
	rows, err := f.db.QueryContext(context.Background(), `SELECT rowid FROM `+table+` WHERE `+table+` MATCH ? ORDER BY rowid`, expr) //nolint:gosec // one of the two FTS table constants
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

var (
	sharedFixtureOnce sync.Once
	sharedFixture     *queryFixture
)

// fuzzFixture is the one fixture a fuzz process shares (building it per input would dominate the run).
func fuzzFixture(tb testing.TB) *queryFixture {
	sharedFixtureOnce.Do(func() { sharedFixture = newQueryFixture(tb) })
	return sharedFixture
}

// exprShapeError checks that match is built only from the pieces the compiler may emit: quoted strings
// (a quote inside is doubled; the text is normalization-stable and holds no control character), parentheses,
// AND, OR, NOT, one blank and a star after a quoted string, and, only when a column was requested, the one
// leading `<column> : (` with its closing parenthesis. It returns nil when the expression has that shape.
func exprShapeError(match, column string) error {
	s := match
	if column != "" {
		prefix := column + " : ("
		if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, ")") {
			return fmt.Errorf("a %s filter was requested but the expression does not have the form %s...)", column, prefix)
		}
		s = s[len(prefix) : len(s)-1]
	}
	depth := 0
	afterString := false
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case '"':
			j := i + 1
			var text strings.Builder
			for {
				if j >= len(s) {
					return fmt.Errorf("unterminated quoted string at byte %d", i)
				}
				if s[j] == '"' {
					if j+1 < len(s) && s[j+1] == '"' {
						text.WriteByte('"')
						j += 2
						continue
					}
					break
				}
				text.WriteByte(s[j])
				j++
			}
			t := text.String()
			if t == "" || evidence.NormalizeText(t) != t {
				return fmt.Errorf("quoted text %q is empty or not normalization-stable", t)
			}
			for _, r := range t {
				if unicode.IsControl(r) {
					return fmt.Errorf("quoted text %q holds a control character", t)
				}
			}
			i, afterString = j+1, true
			continue
		case '(':
			depth++
		case ')':
			if depth--; depth < 0 {
				return fmt.Errorf("unbalanced parenthesis at byte %d", i)
			}
		case ' ':
			rest := s[i+1:]
			switch {
			case afterString && strings.HasPrefix(rest, "*") && (len(rest) == 1 || rest[1] == ' ' || rest[1] == ')'):
				i += 2 // " *"
				afterString = false
				continue
			case strings.HasPrefix(rest, "AND "):
				i += 5
			case strings.HasPrefix(rest, "OR "):
				i += 4
			case strings.HasPrefix(rest, "NOT "):
				i += 5
			default:
				return fmt.Errorf("unexpected blank at byte %d in %q", i, s)
			}
			afterString = false
			continue
		default:
			return fmt.Errorf("unexpected byte %q at %d in %q", c, i, s)
		}
		afterString = false
		i++
	}
	if depth != 0 {
		return fmt.Errorf("unbalanced parentheses in %q", s)
	}
	return nil
}
