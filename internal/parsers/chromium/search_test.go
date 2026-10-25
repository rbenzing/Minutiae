package chromium_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	searchDDL = "create table keyword_search_terms(keyword_id integer, url_id integer, term longvarchar, normalized_term longvarchar)"
	tMemISO   = "2023-05-01T10:00:00Z"
)

// searchDB is a database of urls, visits and keyword_search_terms.
func searchDB() (*sqlitetest.Builder, *sqlitetest.Table, *sqlitetest.Table) {
	b, u, _ := memDB(1024)
	return b, u, b.CreateTable("keyword_search_terms", searchDDL)
}

// 4F-R10: a search has no time of its own; it takes its URL's last visit and says so, and it never states an engine.
func TestSearchTimeComesFromTheURLRowAndSaysSo(t *testing.T) {
	b, u, s := searchDB()
	u.Insert(1, int64(1), "https://example.com/a", "A", int64(1), int64(0), tMem, int64(0))
	u.Insert(3, int64(3), "https://example.net/never", "N", int64(1), int64(0), int64(0), int64(0))
	s.Insert(1, int64(2), int64(1), "Minutiae Forensics", "minutiae forensics")
	s.Insert(2, int64(2), int64(999), "dangling term", "dangling term")
	s.Insert(3, int64(2), int64(3), "sentinel time", "sentinel time")
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	r := recFor(t, res, "keyword_search_terms", 1)
	if r.Type != "web_search" || r.Time == nil || r.Time.T.Format(time.RFC3339) != tMemISO {
		t.Fatalf("search 1 = %+v, want a web_search at %s", r, tMemISO)
	}
	p := r.Payload
	if p["time_source"] != "urls.last_visit_time" || p["url"] != "https://example.com/a" || p["engine_id"] != "2" ||
		p["term"] != "Minutiae Forensics" || p["normalized_term"] != "minutiae forensics" || p["browser"] != "chrome" {
		t.Errorf("payload %v", p)
	}
	if _, ok := p["engine"]; ok {
		t.Errorf("the engine is stated (%v): engine names live in another database", p["engine"])
	}
	if r.Body != "Minutiae Forensics" {
		t.Errorf("body %q, want the term", r.Body)
	}
	if raw := asMap(p["raw"]); raw["profile"] != "Default" || raw["package"] != "com.android.chrome" {
		t.Errorf("raw %v: the profile and package belong in raw on a search (4F-C1)", raw)
	}

	d := recFor(t, res, "keyword_search_terms", 2)
	raw := asMap(d.Payload["raw"])
	if d.Time != nil || raw["url_state"] != "no-urls-row" {
		t.Errorf("dangling search time %v raw %v, want no time and url_state no-urls-row", d.Time, raw)
	}
	for _, k := range []string{"url", "time_source"} {
		if _, ok := d.Payload[k]; ok {
			t.Errorf("dangling search states %s", k)
		}
	}

	z := recFor(t, res, "keyword_search_terms", 3)
	raw = asMap(z.Payload["raw"])
	if z.Time != nil || raw["last_visit_time_status"] != "sentinel" || z.Payload["url"] != "https://example.net/never" {
		t.Errorf("sentinel search time %v raw %v payload %v", z.Time, raw, z.Payload)
	}
	if _, ok := z.Payload["time_source"]; ok {
		t.Error("a search without a time names a time source")
	}
}

// 4F-R10 and 4F-R30: a urls row that cannot answer (its id is not the rowid) leaves the search's url undecided, said
// so in raw.join_undecided; it is neither "no urls row" nor a url.
func TestSearchWithAnUndecidableURLsRowSaysSo(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	u := b.CreateTable("urls", "create table urls(id text, url longvarchar, title longvarchar, last_visit_time integer not null)")
	b.CreateTable("visits", visitsDDL)
	s := b.CreateTable("keyword_search_terms", searchDDL)
	u.Insert(1, "other", "https://a.example/", "A", tMem)
	s.Insert(1, int64(2), int64(1), "term", "term")
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	r := recFor(t, res, "keyword_search_terms", 1)
	raw := asMap(r.Payload["raw"])
	if j, _ := raw["join_undecided"].([]any); len(j) != 1 || j[0] != "urls" {
		t.Errorf("raw %v, want join_undecided [urls]", raw)
	}
	if _, ok := raw["url_state"]; ok || r.Time != nil || r.Payload["url"] != nil {
		t.Errorf("an undecided join states a url state, url or time: %v %v", raw, r.Time)
	}
}

// 4F-R10: an empty or unusable term is a counted warning naming the term; a storage class that is not the declared
// one is stated, never read as a value.
func TestUnusableSearchTermIsAWarningAndWrongClassesAreStated(t *testing.T) {
	b, u, s := searchDB()
	u.Insert(1, int64(1), "https://example.com/a", "A", int64(1), int64(0), tMem, int64(0))
	s.Insert(1, int64(2), int64(1), "", "x")
	s.Insert(2, int64(2), int64(1), nil, "x")
	s.Insert(3, int64(2), int64(1), []byte{1, 2}, "x")
	s.Insert(4, "text", int64(1), "kept term", int64(7))
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(res.Warnings) != 3 {
		t.Fatalf("%d warnings %+v, want 3", len(res.Warnings), res.Warnings)
	}
	for i, w := range res.Warnings {
		if want := "sqlite:table=keyword_search_terms;rowid=" + strconv.Itoa(i+1); w.Locator != want || !strings.Contains(w.Reason, "term") {
			t.Errorf("warning %d = %+v, want %s naming term", i, w, want)
		}
	}
	r := recFor(t, res, "keyword_search_terms", 4)
	raw := asMap(r.Payload["raw"])
	unk := asMap(raw["unknown_columns"])
	if unk["keyword_id"] != "type-mismatch" || unk["normalized_term"] != "type-mismatch" {
		t.Errorf("unknown_columns %v, want keyword_id and normalized_term type-mismatch", unk)
	}
	for _, k := range []string{"engine_id", "normalized_term"} {
		if _, ok := r.Payload[k]; ok {
			t.Errorf("payload states %s from a cell of the wrong class", k)
		}
	}
}

// 4F-R10: a url_id that is not an integer is stated in unknown_columns and joins nothing: no url state, no join
// marker, no url and no time.
func TestSearchWithANonIntegerURLIDStatesItAndJoinsNothing(t *testing.T) {
	b, u, s := searchDB()
	u.Insert(1, int64(1), "https://example.com/a", "A", int64(1), int64(0), tMem, int64(0))
	s.Insert(1, int64(2), "one", "term", "term")
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	r := recFor(t, res, "keyword_search_terms", 1)
	raw := asMap(r.Payload["raw"])
	if asMap(raw["unknown_columns"])["url_id"] != "type-mismatch" {
		t.Errorf("raw %v, want unknown_columns.url_id type-mismatch", raw)
	}
	for _, k := range []string{"url_state", "join_undecided", "url_id"} {
		if _, ok := raw[k]; ok {
			t.Errorf("raw states %s for a url_id that joins nothing", k)
		}
	}
	if r.Time != nil || r.Payload["url"] != nil {
		t.Errorf("time %v url %v, want neither", r.Time, r.Payload["url"])
	}
}
