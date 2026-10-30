package chromium_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	urlsDDL   = "create table urls(id integer primary key, url longvarchar, title longvarchar, visit_count integer default 0 not null, typed_count integer default 0 not null, last_visit_time integer not null, hidden integer default 0 not null)"
	visitsDDL = "create table visits(id integer primary key, url integer not null, visit_time integer not null, from_visit integer, transition integer default 0 not null, visit_duration integer default 0 not null)"
	metaDDL   = "create table meta(key text, value text)"

	// 2023-05-01T10:00:00Z in WebKit microseconds: (1682935200 + 11644473600) * 1e6
	tMem = int64(13327408800000000)
)

// memDB is a database of the two mandatory tables: rows are given as insert calls.
func memDB(page int) (*sqlitetest.Builder, *sqlitetest.Table, *sqlitetest.Table) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: page})
	u := b.CreateTable("urls", urlsDDL)
	v := b.CreateTable("visits", visitsDDL)
	return b, u, v
}

func runMem(t *testing.T, db []byte) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": db}}.Bundle(h, remoteChrome)
	p := chromium.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	noViolations(t, res)
	return res
}

func recFor(t *testing.T, res parsertest.Result, table string, rowid int64) records.Record {
	t.Helper()
	loc := fmt.Sprintf("sqlite:table=%s;rowid=%d", table, rowid)
	for _, r := range res.Records {
		if r.Locator == loc || strings.HasPrefix(r.Locator, loc+";") {
			return r
		}
	}
	t.Fatalf("no record for %s", loc)
	return records.Record{}
}

func hasRecFor(res parsertest.Result, table string) bool {
	for _, r := range res.Records {
		if strings.HasPrefix(r.Locator, "sqlite:table="+table+";") {
			return true
		}
	}
	return false
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// 4F-R4: a visit whose urls row is absent, undecidable or holds no usable url is a counted warning, never a record
// with an invented URL.
func TestDanglingVisitURLIsAWarningNeverARecord(t *testing.T) {
	b, u, v := memDB(1024)
	u.Insert(1, int64(1), "https://a.example/", "A", int64(1), int64(0), tMem, int64(0))
	v.Insert(1, int64(1), int64(999), tMem, nil, int64(0), int64(0))
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if hasRecFor(res, "visits") {
		t.Errorf("a record for the dangling visit: %+v", res.Records)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Locator != "sqlite:table=visits;rowid=1" || !strings.Contains(res.Warnings[0].Reason, "urls") {
		t.Errorf("warnings %+v, want one for sqlite:table=visits;rowid=1 naming urls", res.Warnings)
	}
	if res.Notes["unmapped_rows.visits"] != "1" {
		t.Errorf("note unmapped_rows.visits = %q, want 1", res.Notes["unmapped_rows.visits"])
	}
	// the url nothing visits is a visitless record, decided by a clean miss
	if raw := asMap(recFor(t, res, "urls", 1).Payload["raw"]); raw["visitless"] != true {
		t.Errorf("urls 1 raw %v, want visitless", raw)
	}
}

// 4F-R4 and 4F-R30: every way the urls row of a visit can be unusable is a warning that names urls.
func TestVisitWithoutAUsableURLsRowIsAWarning(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	u := b.CreateTable("urls", urlsDDL)
	v := b.CreateTable("visits", visitsDDL)
	u.Insert(1, int64(1), "", "Empty", int64(1), int64(0), tMem, int64(0))
	u.Insert(2, int64(2), nil, "Null", int64(1), int64(0), tMem, int64(0))
	u.Insert(3, int64(3), []byte{1, 2}, "Blob", int64(1), int64(0), tMem, int64(0))
	v.Insert(1, int64(1), int64(1), tMem, nil, int64(0), int64(0))
	v.Insert(2, int64(2), int64(2), tMem, nil, int64(0), int64(0))
	v.Insert(3, int64(3), int64(3), tMem, nil, int64(0), int64(0))
	v.Insert(4, int64(4), "x", tMem, nil, int64(0), int64(0))
	v.Insert(5, int64(5), nil, tMem, nil, int64(0), int64(0))
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if hasRecFor(res, "visits") {
		t.Errorf("a visit record from an unusable urls row: %+v", res.Records)
	}
	if len(res.Warnings) != 5 {
		t.Fatalf("%d warnings %+v, want 5", len(res.Warnings), res.Warnings)
	}
	for i, w := range res.Warnings {
		if want := fmt.Sprintf("sqlite:table=visits;rowid=%d", i+1); w.Locator != want || !strings.Contains(w.Reason, "urls") {
			t.Errorf("warning %d = %+v, want %s naming urls", i, w, want)
		}
	}
	if res.Notes["unmapped_rows.visits"] != "5" {
		t.Errorf("note unmapped_rows.visits = %q, want 5", res.Notes["unmapped_rows.visits"])
	}
}

// 4F-R30: a join by rowid is only a join on id when id is the rowid alias; a found row whose id cell differs is not
// used.
func TestURLsRowWhoseIDIsNotTheRowidIsRefused(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	u := b.CreateTable("urls", "create table urls(id text, url longvarchar, title longvarchar, last_visit_time integer not null)")
	v := b.CreateTable("visits", visitsDDL)
	u.Insert(1, "other", "https://a.example/", "A", tMem)
	v.Insert(1, int64(1), int64(1), tMem, nil, int64(0), int64(0))
	// Parse is run without a Probe, as it is when a companion file defers the probe's alias check to Parse
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if hasRecFor(res, "visits") {
		t.Errorf("a visit record through a urls row whose id is not its rowid: %+v", res.Records)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || (w.Locator == "sqlite:table=visits;rowid=1" && strings.Contains(w.Reason, "rowid"))
	}
	if !found {
		t.Errorf("warnings %+v, want one for visits rowid 1 saying the id column is not the rowid", res.Warnings)
	}
}

// Review focus 2: history cleared from visits while urls keeps the rows. A url is claimed visitless only on a decided
// miss; a damaged visits index never yields visitless: true.
func TestVisitlessIsNeverClaimedWhenTheVisitsJoinIsUndecided(t *testing.T) {
	build := func(damage bool) (parsertest.Result, int) {
		b, u, v := memDB(512)
		u.Insert(1, int64(1), "https://visited.example/", "Visited", int64(200), int64(0), tMem, int64(0))
		u.Insert(2, int64(2), "https://cleared.example/", "Cleared", int64(0), int64(0), tMem, int64(0))
		for i := int64(1); i <= 200; i++ {
			v.Insert(i, i, int64(1), tMem+i*1_000_000, nil, int64(1), int64(0))
		}
		leaves := v.Leaves()
		if len(leaves) < 4 {
			t.Fatalf("visits has %d leaves, want several", len(leaves))
		}
		db := b.Bytes()
		if damage {
			pg := leaves[1]
			page := db[int(pg-1)*512:][:512]
			if page[0] != 0x0d { // a table leaf of the visits tree
				t.Fatalf("page %d of visits has type %#x, not a table leaf", pg, page[0])
			}
			page[3], page[4] = 0, 0 // zero the cell count: the leaf's rows are lost
		}
		return runMem(t, db), len(leaves)
	}
	intact, _ := build(false)
	if intact.Err != nil || len(intact.Warnings) != 0 {
		t.Fatalf("intact: err %v warnings %+v", intact.Err, intact.Warnings)
	}
	r := recFor(t, intact, "urls", 2)
	raw := asMap(r.Payload["raw"])
	if raw["visitless"] != true || raw["join_undecided"] != nil {
		t.Errorf("intact urls 2 raw %v, want visitless and no join_undecided", raw)
	}
	for _, rec := range intact.Records {
		if rec.Locator == "sqlite:table=urls;rowid=1" {
			t.Errorf("urls 1 has 200 visits and must give no record: %+v", rec)
		}
	}

	damaged, _ := build(true)
	if damaged.Err != nil {
		t.Fatal(damaged.Err)
	}
	r = recFor(t, damaged, "urls", 2)
	raw = asMap(r.Payload["raw"])
	if _, claimed := raw["visitless"]; claimed {
		t.Errorf("damaged: urls 2 claims visitless on an index that lost rows: %v", raw)
	}
	if und, _ := raw["join_undecided"].([]any); !slices.Equal(und, []any{"visits"}) {
		t.Errorf("damaged: urls 2 join_undecided = %v, want [visits]", raw["join_undecided"])
	}
}

func TestLongURLIsCappedInBodyAndKeptInPayload(t *testing.T) {
	// the committed data URL: 100 KiB in the payload, 64 KiB of it in the body
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "current")
	b := fx.Bundle(h, remoteChrome)
	p := chromium.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	noViolations(t, res)
	r := recFor(t, res, "visits", 3)
	url, _ := r.Payload["url"].(string)
	const title = "Big data URL"
	if len(url) != 100*1024 {
		t.Errorf("payload url is %d bytes, want %d", len(url), 100*1024)
	}
	if want := 64<<10 + 1 + len(title); len(r.Body) != want {
		t.Errorf("body is %d bytes, want %d (the url cut at 64 KiB, a newline, the title)", len(r.Body), want)
	}
	raw := asMap(r.Payload["raw"])
	if raw["body_truncated"] != true || raw["body_total_bytes"] != int64(100*1024+1+len(title)) {
		t.Errorf("raw body_truncated %v body_total_bytes %v, want true and %d", raw["body_truncated"], raw["body_total_bytes"], 100*1024+1+len(title))
	}

	// 2 MiB + 10 bytes of URL: cut in the payload, counted in raw
	const prefix = "https://example.com/"
	big := prefix + strings.Repeat("a", 2<<20+10-len(prefix))
	mb, u, v := memDB(4096)
	u.Insert(1, int64(1), big, "Huge", int64(1), int64(0), tMem, int64(0))
	v.Insert(1, int64(1), int64(1), tMem, nil, int64(0), int64(0))
	h2 := parsertest.NewHarness(t)
	bun := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": mb.Bytes()}}.Bundle(h2, remoteChrome)
	res = h2.Run(p, bun.Input(h2, p, false))
	r = recFor(t, res, "visits", 1)
	url, _ = r.Payload["url"].(string)
	raw = asMap(r.Payload["raw"])
	if len(url) != 2<<20 || raw["url_truncated"] != true || raw["url_total_bytes"] != int64(2<<20+10) {
		t.Errorf("payload url %d bytes, raw url_truncated %v url_total_bytes %v; want %d, true, %d", len(url), raw["url_truncated"], raw["url_total_bytes"], 2<<20, 2<<20+10)
	}
	if fl, _ := raw["text_flags"].([]any); !slices.Equal(fl, []any{"url"}) {
		t.Errorf("text_flags %v, want [url]", raw["text_flags"])
	}
	if len(r.Body) != 64<<10+1+len("Huge") {
		t.Errorf("body %d bytes, want %d", len(r.Body), 64<<10+1+len("Huge"))
	}
}

// 4F-R23 and review focus: a cell of the wrong storage class is unknown, not zero and not absent.
func TestWrongStorageClassIsUnknownNotZero(t *testing.T) {
	b, u, v := memDB(1024)
	u.Insert(1, int64(1), "https://a.example/", "T", "many", int64(0), tMem, int64(2))
	v.Insert(1, int64(1), int64(1), "yesterday", nil, 1.5, []byte{1, 2, 3})
	res := runMem(t, b.Bytes())
	if res.Err != nil || len(res.Warnings) != 0 {
		t.Fatalf("err %v warnings %+v", res.Err, res.Warnings)
	}
	r := recFor(t, res, "visits", 1)
	if r.Time != nil {
		t.Errorf("time %v from a TEXT visit_time", r.Time)
	}
	for _, k := range []string{"transition", "transition_qualifiers", "url_visit_count", "hidden", "duration_s"} {
		if _, has := r.Payload[k]; has {
			t.Errorf("payload states %s = %v from a cell of the wrong class", k, r.Payload[k])
		}
	}
	if r.Payload["typed_count"] != int64(0) {
		t.Errorf("typed_count %v: a real zero count is stated", r.Payload["typed_count"])
	}
	raw := asMap(r.Payload["raw"])
	unknown := asMap(raw["unknown_columns"])
	for _, c := range []string{"visit_time", "transition", "visit_duration"} {
		if unknown[c] != "type-mismatch" {
			t.Errorf("unknown_columns.%s = %v, want type-mismatch", c, unknown[c])
		}
	}
	joined := asMap(raw["joined_unknown_columns"])
	if joined["urls.visit_count"] != "type-mismatch" || joined["urls.hidden"] != "not-boolean" || len(joined) != 2 {
		t.Errorf("joined_unknown_columns = %v, want urls.visit_count type-mismatch and urls.hidden not-boolean", joined)
	}
}

// A WAL-resident visit points into the WAL artifact, with its frame in the locator (T48).
func TestWALVisitPointsIntoTheWALArtifact(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "current-wal")
	b := fx.Bundle(h, remoteChrome)
	p := chromium.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	noViolations(t, res)
	n := 0
	for _, w := range fx.Expect.Records {
		if w.File != "wal" || w.Table != "visits" {
			continue
		}
		n++
		r := recFor(t, res, "visits", w.Rowid)
		if r.ArtifactID != b.ByRole["wal"].ID || r.SourcePath != remoteChrome+"-wal" || !strings.Contains(r.Locator, ";wal_frame=") {
			t.Errorf("visits rowid %d: artifact %q source %q locator %q; want the WAL artifact %q at %s-wal with a wal_frame", w.Rowid, r.ArtifactID, r.SourcePath, r.Locator, b.ByRole["wal"].ID, remoteChrome)
		}
	}
	if n < 3 {
		t.Errorf("%d WAL visits checked, want at least 3", n)
	}
}

// 4F-C2: the oracle states a summary only for plain ASCII; the bidi, ZWJ and over-long cases are pinned here against
// common.SummarizeChars, the one routine a summary goes through.
func TestVisitSummaryIsTheSharedSummarizer(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "current")
	b := fx.Bundle(h, remoteChrome)
	p := chromium.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	noViolations(t, res)
	r := recFor(t, res, "visits", 21) // urls 8: a right-to-left override and a zero-width joiner in the title
	title, _ := r.Payload["title"].(string)
	if want := "Re\u202etsooB \U0001F469\u200d\U0001F4BB"; title != want {
		t.Fatalf("title %q, want %q", title, want)
	}
	if want := common.SummarizeChars(title, 80); r.Summary != want || !strings.Contains(r.Summary, "<U+202E>") || !strings.Contains(r.Summary, "<U+200D>") {
		t.Errorf("summary %q, want %q (format characters shown as code points)", r.Summary, want)
	}

	// a title over 80 characters is cut by the same routine; with no title the url is summarised
	mb, u, v := memDB(1024)
	long := strings.Repeat("abcdefghij", 20)
	u.Insert(1, int64(1), "https://a.example/", long, int64(1), int64(0), tMem, int64(0))
	u.Insert(2, int64(2), "https://b.example/\u202every", "", int64(1), int64(0), tMem, int64(0))
	v.Insert(1, int64(1), int64(1), tMem, nil, int64(0), int64(0))
	v.Insert(2, int64(2), int64(2), tMem, nil, int64(0), int64(0))
	res = runMem(t, mb.Bytes())
	if got, want := recFor(t, res, "visits", 1).Summary, common.SummarizeChars(long, 80); got != want || len([]rune(got)) != 80 {
		t.Errorf("long title summary %q, want %q", got, want)
	}
	if got, want := recFor(t, res, "visits", 2).Summary, common.SummarizeChars("https://b.example/\u202every", 80); got != want || !strings.Contains(got, "<U+202E>") {
		t.Errorf("url summary %q, want %q", got, want)
	}
}

// 4F-R21: the meta step emits no record and notes its two version keys.
func TestMetaNotes(t *testing.T) {
	build := func(rows ...[]any) parsertest.Result {
		b, u, v := memDB(1024)
		u.Insert(1, int64(1), "https://a.example/", "A", int64(1), int64(0), tMem, int64(0))
		v.Insert(1, int64(1), int64(1), tMem, nil, int64(0), int64(0))
		m := b.CreateTable("meta", metaDDL)
		for i, r := range rows {
			m.Insert(int64(i+1), r...)
		}
		return runMem(t, b.Bytes())
	}
	res := build([]any{"version", int64(70)}, []any{"last_compatible_version", "16"}, []any{"mmap_status", "-1"})
	if res.Err != nil || res.Notes["meta.version"] != "70" || res.Notes["meta.last_compatible_version"] != "16" || res.Notes["meta.mmap_status"] != "" {
		t.Errorf("err %v notes %v", res.Err, res.Notes)
	}
	for _, c := range []struct {
		val  any
		want string
	}{
		{[]byte{1}, "non-numeric"},
		{"1234567890123456", "1234567890123456"},
		{"12345678901234567", "non-numeric"},
		{"1e3", "non-numeric"},
		{"", "non-numeric"},
		{"+5", "non-numeric"},
		{1.5, "non-numeric"},
	} {
		res = build([]any{"version", c.val})
		if got := res.Notes["meta.version"]; got != c.want {
			t.Errorf("value %#v: note %q, want %q", c.val, got, c.want)
		}
	}
	res = build([]any{"version", nil})
	if _, has := res.Notes["meta.version"]; has {
		t.Errorf("a NULL value states a note: %v", res.Notes)
	}
	for _, r := range res.Records {
		if strings.HasPrefix(r.Locator, "sqlite:table=meta;") {
			t.Errorf("the meta table gave a record: %s", r.Locator)
		}
	}
}

func noViolations(t *testing.T, res parsertest.Result) {
	t.Helper()
	for _, v := range res.Violations {
		t.Errorf("contract violation: %v", v)
	}
}

// 4F-C5 (amends 4F-R21): a meta value the decoder cannot vouch for (here its overflow chain is broken) is noted as
// "unknown", never as "non-numeric".
func TestUndecodableMetaValueIsUnknownNotNonNumeric(t *testing.T) {
	b, u, v := memDB(512)
	u.Insert(1, int64(1), "https://a.example/", "A", int64(1), int64(0), tMem, int64(0))
	v.Insert(1, int64(1), int64(1), tMem, nil, int64(0), int64(0))
	m := b.CreateTable("meta", metaDDL)
	m.Insert(1, "version", strings.Repeat("7", 1500))
	m.Insert(2, "last_compatible_version", "16")
	ov := m.Overflow(1)
	if len(ov) == 0 {
		t.Fatal("the long meta value has no overflow page")
	}
	db := b.Bytes()
	page := db[int(ov[0]-1)*512:][:512]
	page[0], page[1], page[2], page[3] = 0x7f, 0xff, 0xff, 0xff // the chain continues at a page that does not exist
	res := runMem(t, db)
	if got := res.Notes["meta.version"]; got != "unknown" {
		t.Errorf("meta.version = %q, want unknown (notes %v, warnings %+v)", got, res.Notes, res.Warnings)
	}
	if got := res.Notes["meta.last_compatible_version"]; got != "16" {
		t.Errorf("the intact row must still be noted: %q", got)
	}
}

// A urls table of a variant without title and typed_count: the joined cells are stated as missing, not as absent
// values, and the count that is there is still stated.
func TestJoinedColumnsTheVariantLacksAreStatedMissing(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	u := b.CreateTable("urls", "create table urls(id integer primary key, url longvarchar, visit_count integer, last_visit_time integer not null, hidden integer)")
	v := b.CreateTable("visits", visitsDDL)
	u.Insert(1, int64(1), "https://a.example/", int64(4), tMem, int64(0))
	v.Insert(1, int64(1), int64(1), tMem, nil, int64(0), int64(0))
	res := runMem(t, b.Bytes())
	if res.Err != nil || len(res.Warnings) != 0 {
		t.Fatalf("err %v warnings %+v", res.Err, res.Warnings)
	}
	r := recFor(t, res, "visits", 1)
	joined := asMap(asMap(r.Payload["raw"])["joined_unknown_columns"])
	if joined["urls.title"] != "column-missing" || joined["urls.typed_count"] != "column-missing" || len(joined) != 2 {
		t.Errorf("joined_unknown_columns = %v, want urls.title and urls.typed_count column-missing", joined)
	}
	if r.Payload["url_visit_count"] != int64(4) {
		t.Errorf("url_visit_count %v, want 4", r.Payload["url_visit_count"])
	}
	if _, has := r.Payload["typed_count"]; has {
		t.Errorf("typed_count stated from a column the variant lacks: %v", r.Payload["typed_count"])
	}
}

// 4h-R5: a REAL in an integer column of the joined urls row keeps its bits under the joined key.
func TestJoinedRealInAnIntegerColumnKeepsItsBits(t *testing.T) {
	b, u, v := memDB(1024)
	u.Insert(1, int64(1), "https://a.example/", "T", 1.25, int64(0), tMem, int64(0))
	v.Insert(1, int64(1), int64(1), tMem, nil, int64(0), int64(0))
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	raw := asMap(recFor(t, res, "visits", 1).Payload["raw"])
	if got := asMap(raw["joined_unknown_columns"])["urls.visit_count"]; got != "type-mismatch" {
		t.Errorf("joined_unknown_columns.urls.visit_count = %v, want type-mismatch", got)
	}
	if got := raw["urls.visit_count_ieee"]; got != "3ff4000000000000" {
		t.Errorf("raw[urls.visit_count_ieee] = %v, want 3ff4000000000000", got)
	}
	if _, has := raw["urls.visit_count"]; has {
		t.Error("raw states a urls.visit_count number beside its bits")
	}
}

// 4h-R5: a visitless urls row is the mapped row itself, so its key carries no table prefix.
func TestVisitlessURLRealInAnIntegerColumnKeepsItsBits(t *testing.T) {
	b, u, _ := memDB(1024)
	u.Insert(1, int64(1), "https://a.example/", "T", int64(0), int64(0), 1.25, int64(0))
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	raw := asMap(recFor(t, res, "urls", 1).Payload["raw"])
	if got := asMap(raw["unknown_columns"])["last_visit_time"]; got != "type-mismatch" {
		t.Errorf("unknown_columns.last_visit_time = %v, want type-mismatch", got)
	}
	if got := raw["last_visit_time_ieee"]; got != "3ff4000000000000" {
		t.Errorf("raw.last_visit_time_ieee = %v, want 3ff4000000000000", got)
	}
}
