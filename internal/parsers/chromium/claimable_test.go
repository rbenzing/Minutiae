package chromium_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// liveRows counts the rows of table in the bundle's database the way a typed recovery pass would see them.
func liveRows(t *testing.T, h *parsertest.Harness, b parsertest.Bundle, table string) int {
	t.Helper()
	p := chromium.Parser{}
	in := b.Input(h, p, false)
	files := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok {
		files.WAL, files.WALSize = a.R, a.Size
	}
	db, err := sqlitedb.Open(t.Context(), files, in.Budget)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Release()
	tbl, err := db.Table(t.Context(), table, nil, nil)
	if errors.Is(err, sqlitedb.ErrNoSuchTable) {
		return 0 // a variant without the optional table has no rows to account for
	}
	if err != nil {
		t.Fatalf("table %s: %v", table, err)
	}
	n := 0
	if err := tbl.Scan(t.Context(), func(sqlitedb.Row) error { n++; return nil }); err != nil {
		t.Fatalf("scan %s: %v", table, err)
	}
	return n
}

// countFor is the number of records and the number of warnings whose locator names table.
func countFor(res parsertest.Result, table string) (recs, warns int) {
	prefix := "sqlite:table=" + table + ";"
	for _, r := range res.Records {
		if strings.HasPrefix(r.Locator, prefix) {
			recs++
		}
	}
	for _, w := range res.Warnings {
		if strings.HasPrefix(w.Locator, prefix) {
			warns++
		}
	}
	return recs, warns
}

// hostileDB is a database whose claimable tables are damaged in every way a mapping can meet: dangling, empty,
// NULL and wrong-class keys, unusable terms, and downloads with and without a target or a chain.
func hostileDB() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	u := b.CreateTable("urls", urlsDDL)
	v := b.CreateTable("visits", visitsDDL)
	s := b.CreateTable("keyword_search_terms", searchDDL)
	d := b.CreateTable("downloads", dlDDL)
	c := b.CreateTable("downloads_url_chains", chainDDL)
	u.Insert(1, int64(1), "https://a.example/", "A", int64(1), int64(0), tMem, int64(0))
	u.Insert(2, int64(2), "", "Empty", int64(1), int64(0), tMem, int64(0))
	v.Insert(1, int64(1), int64(1), tMem, nil, int64(0), int64(0))  // good
	v.Insert(2, int64(2), int64(2), tMem, nil, int64(0), int64(0))  // its urls row holds no url
	v.Insert(3, int64(3), int64(99), tMem, nil, int64(0), int64(0)) // dangling
	v.Insert(4, int64(4), "x", tMem, nil, int64(0), int64(0))       // text key
	v.Insert(5, int64(5), nil, tMem, nil, int64(0), int64(0))       // NULL key
	s.Insert(1, int64(2), int64(1), "good term", "good term")       // good
	s.Insert(2, int64(2), int64(1), "", "")                         // empty term
	s.Insert(3, int64(2), int64(99), "dangling", "dangling")        // a record: no url, no time
	s.Insert(4, int64(2), "x", "text url id", "text url id")        // a record: the join is not run
	s.Insert(5, int64(2), int64(1), nil, nil)                       // NULL term
	insertDL(d, 1, nil)                                             // good, with a chain
	insertDL(d, 2, map[string]any{"target_path": ""})               // no target, no chain
	insertDL(d, 3, map[string]any{"target_path": nil})              // no target, a chain url
	insertDL(d, 4, nil)                                             // a target, no chain
	c.Insert(1, int64(1), int64(0), "https://a.example/1")
	c.Insert(2, int64(3), int64(0), "https://a.example/3")
	return b.Bytes()
}

// Only total mappings are claimable (spec 12.3 point 6): every row of a claimed table gives a record or a counted
// warning, never nothing. ClaimableTables is the hand-off to plan 4G.
func TestClaimableTablesAreTotal(t *testing.T) {
	want := []string{"visits", "keyword_search_terms", "downloads"}
	if got := chromium.ClaimableTables(); !slices.Equal(got, want) {
		t.Fatalf("ClaimableTables = %v", got)
	}
	check := func(t *testing.T, h *parsertest.Harness, b parsertest.Bundle) parsertest.Result {
		p := chromium.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		noViolations(t, res)
		if res.Err != nil {
			t.Fatalf("err %v", res.Err)
		}
		for _, table := range want {
			recs, warns := countFor(res, table)
			if live := liveRows(t, h, b, table); recs+warns != live {
				t.Errorf("%s: %d records and %d warnings for %d rows", table, recs, warns, live)
			}
		}
		return res
	}
	for _, name := range variants {
		t.Run(name, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			res := check(t, h, parsertest.LoadFixture(t, "testdata", name).Bundle(h, remoteFor(name)))
			if name == "current" {
				for _, table := range want {
					if recs, _ := countFor(res, table); recs == 0 {
						t.Errorf("current gave no %s record: the check above proved nothing", table)
					}
				}
			}
		})
	}
	t.Run("hostile", func(t *testing.T) {
		h := parsertest.NewHarness(t)
		b := parsertest.Fixture{Name: "hostile", Files: map[string][]byte{"db": hostileDB()}}.Bundle(h, remoteChrome)
		res := check(t, h, b)
		for table, w := range map[string][2]int{"visits": {1, 4}, "keyword_search_terms": {3, 2}, "downloads": {3, 1}} {
			if recs, warns := countFor(res, table); recs != w[0] || warns != w[1] {
				t.Errorf("%s: %d records and %d warnings, want %d and %d", table, recs, warns, w[0], w[1])
			}
		}
	})
}

// A claim is a table of one mapping (4F-R34): ClaimsOutsideMappings, the check the registry applies to a Meta,
// finds nothing when the claimable tables are claimed.
func TestClaimableTablesAreMappedTables(t *testing.T) {
	p := chromium.Parser{}
	var claims []parse.TableClaim
	for _, tbl := range chromium.ClaimableTables() {
		claims = append(claims, parse.TableClaim{Role: "db", Table: tbl})
		n := 0
		for _, mp := range p.Mappings() {
			if mp.Table == tbl {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s is the table of %d mappings, want exactly one", tbl, n)
		}
	}
	m := p.Meta()
	m.Claims = claims
	if bad := parse.ClaimsOutsideMappings(m, p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
	m.Claims = append(m.Claims, parse.TableClaim{Role: "db", Table: "segments"})
	if bad := parse.ClaimsOutsideMappings(m, p.Mappings()); len(bad) != 1 {
		t.Errorf("an unmapped claim must be found: %v", bad)
	}
}
