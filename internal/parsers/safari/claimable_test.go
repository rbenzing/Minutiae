package safari_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/parsers/safari"
)

// liveRows counts the rows of table in the bundle's database the way a typed recovery pass would see them.
func liveRows(t *testing.T, h *parsertest.Harness, b parsertest.Bundle, table string) int {
	t.Helper()
	p := safari.Parser{}
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
	if err != nil {
		t.Fatalf("table %s: %v", table, err)
	}
	n := 0
	if err := tbl.Scan(t.Context(), func(sqlitedb.Row) error { n++; return nil }); err != nil {
		t.Fatalf("scan %s: %v", table, err)
	}
	return n
}

// Only total mappings are claimable (spec 12.3 point 6): every row of a claimed table gives a record or a
// counted warning, never nothing. ClaimableTables is the hand-off to plan 4G.
func TestClaimableTablesAreTotal(t *testing.T) {
	if got := safari.ClaimableTables(); !slices.Equal(got, []string{"history_visits"}) {
		t.Fatalf("ClaimableTables = %v", got)
	}
	// A database whose visits are damaged in every way a mapping can meet: a dangling item, an item
	// without a url, a text item key, a NULL item key, and a good visit.
	d := newDB(1024)
	d.item(1, "https://a.example/", int64(1)).item(2, nil, int64(1))
	d.visit(1, 1, tVisit, "good")
	d.visit(2, 2, tVisit, "item without a url")
	d.visit(3, 99, tVisit, "dangling item")
	d.visits.Insert(4, int64(4), "x", tVisit, "text item key", nil, nil, nil)
	d.visits.Insert(5, int64(5), nil, tVisit, "null item key", nil, nil, nil)
	hostile := d.b.Bytes()

	check := func(t *testing.T, h *parsertest.Harness, b parsertest.Bundle) {
		p := safari.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		if len(res.Violations) != 0 {
			t.Errorf("contract violations: %v", res.Violations)
		}
		for _, table := range safari.ClaimableTables() {
			prefix := "sqlite:table=" + table + ";"
			got := 0
			for _, r := range res.Records {
				if strings.HasPrefix(r.Locator, prefix) {
					got++
				}
			}
			for _, w := range res.Warnings {
				if strings.HasPrefix(w.Locator, prefix) {
					got++
				}
			}
			if want := liveRows(t, h, b, table); got == 0 || got != want {
				t.Errorf("%s: %d records and warnings for %d rows", table, got, want)
			}
		}
	}
	for _, name := range variants {
		t.Run(name, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			check(t, h, parsertest.LoadFixture(t, "testdata", name).BundleIOS(h, remoteSafari, logicalSafari))
		})
	}
	t.Run("hostile", func(t *testing.T) {
		h := parsertest.NewHarness(t)
		b := parsertest.Fixture{Name: "hostile", Files: map[string][]byte{"db": hostile}}.BundleIOS(h, remoteSafari, logicalSafari)
		p := safari.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		if len(res.Violations) != 0 || res.Err != nil {
			t.Fatalf("err %v violations %v", res.Err, res.Violations)
		}
		var recs, warns int
		for _, r := range res.Records {
			if strings.HasPrefix(r.Locator, "sqlite:table=history_visits;") {
				recs++
			}
		}
		for _, w := range res.Warnings {
			if strings.HasPrefix(w.Locator, "sqlite:table=history_visits;") {
				warns++
			}
		}
		if recs != 1 || warns != 4 || liveRows(t, h, b, "history_visits") != 5 {
			t.Errorf("%d records and %d warnings for 5 visits (want 1 and 4)", recs, warns)
		}
	})
}

// A claim is a table of one mapping (4F-R34): ClaimsOutsideMappings, the check the registry applies to a
// Meta, finds nothing when the claimable tables are claimed.
func TestClaimableTablesAreMappedTables(t *testing.T) {
	p := safari.Parser{}
	var claims []parse.TableClaim
	for _, tbl := range safari.ClaimableTables() {
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
	m.Claims = append(m.Claims, parse.TableClaim{Role: "db", Table: "daily_visit_counts"})
	if bad := parse.ClaimsOutsideMappings(m, p.Mappings()); len(bad) != 1 {
		t.Errorf("an unmapped claim must be found: %v", bad)
	}
}
