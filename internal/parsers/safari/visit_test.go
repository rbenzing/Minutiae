package safari_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/parsers/safari"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The cases a fixture cannot force: every database here is built in the test.
const (
	itemsDDL  = "create table history_items(id integer primary key autoincrement, url text, visit_count integer, status_code integer default 0)"
	visitsDDL = "create table history_visits(id integer primary key autoincrement, history_item integer, visit_time real, title text, load_successful boolean, redirect_source integer, redirect_destination integer)"
	tombsDDL  = "create table history_tombstones(id integer primary key autoincrement, start_time real, end_time real, url text, generation integer)"

	tVisit = 704628000.0 // 2023-05-01T10:00:00Z in Cocoa seconds
)

type db struct {
	b                   *sqlitetest.Builder
	items, visits, tomb *sqlitetest.Table
}

func newDB(page int) *db {
	b := sqlitetest.New(sqlitetest.Options{PageSize: page})
	return &db{b: b, items: b.CreateTable("history_items", itemsDDL), visits: b.CreateTable("history_visits", visitsDDL), tomb: b.CreateTable("history_tombstones", tombsDDL)}
}

func (d *db) item(id int64, url any, count any) *db {
	d.items.Insert(id, id, url, count, int64(0))
	return d
}

func (d *db) visit(id, item int64, t any, title any) *db {
	d.visits.Insert(id, id, item, t, title, nil, nil, nil)
	return d
}

func runDB(t *testing.T, data []byte) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": data}}.BundleIOS(h, remoteSafari, logicalSafari)
	p := safari.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	for _, v := range res.Violations {
		t.Errorf("contract violation: %v", v)
	}
	return res
}

func rawMap(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

func sub(m map[string]any, key string) map[string]any {
	s, _ := m[key].(map[string]any)
	return s
}

// byLocator indexes the records by "<table>:<rowid>".
func byLocator(t *testing.T, res parsertest.Result) map[string]records.Record {
	t.Helper()
	out := map[string]records.Record{}
	for _, r := range res.Records {
		l := strings.TrimPrefix(r.Locator, "sqlite:table=")
		table, rest, _ := strings.Cut(l, ";rowid=")
		var id int64
		if _, err := fmt.Sscanf(rest, "%d", &id); err != nil {
			t.Fatalf("locator %q: %v", r.Locator, err)
		}
		out[fmt.Sprintf("%s:%d", table, id)] = r
	}
	return out
}

func requireClean(t *testing.T, res parsertest.Result) {
	t.Helper()
	if res.Err != nil || len(res.Warnings) != 0 || len(res.Violations) != 0 {
		t.Fatalf("err %v, warnings %+v, violations %v", res.Err, res.Warnings, res.Violations)
	}
}

func list(m map[string]any, key string) []string {
	var out []string
	l, _ := m[key].([]any)
	for _, v := range l {
		out = append(out, v.(string))
	}
	return out
}

func isTombstone(locator string) bool {
	return strings.HasPrefix(locator, "sqlite:table=history_tombstones;")
}

func webDecode(r records.Record) (web.Visit, error) {
	b, err := json.Marshal(r.Payload)
	if err != nil {
		return web.Visit{}, err
	}
	return web.DecodeVisit(web.PayloadVersion, b)
}

// A damaged visits page loses visit rows: an item whose visits may be among them is neither "has visits"
// nor "visitless" (4F-R9); the intact control states visitless for the same items.
func TestVisitlessItemsNeedADecidedMiss(t *testing.T) {
	lost := map[int64]bool{}
	build := func(damage bool) []byte {
		d := newDB(512)
		for i := int64(1); i <= 300; i++ {
			d.item(i, fmt.Sprintf("https://e.example/%d", i), int64(1))
			d.visit(i, i, tVisit+float64(i), nil)
		}
		for i := int64(301); i <= 305; i++ {
			d.item(i, fmt.Sprintf("https://e.example/%d", i), int64(0))
		}
		data := d.b.Bytes()
		if damage {
			leaves := d.visits.Leaves()
			if len(leaves) < 3 {
				t.Fatalf("%d leaves", len(leaves))
			}
			for i := int64(1); i <= 300; i++ {
				if _, pg, _ := d.visits.CellBytes(i); pg == leaves[1] {
					lost[i] = true
				}
			}
			if len(lost) == 0 || len(lost) == 300 {
				t.Fatalf("%d visits on the damaged leaf", len(lost))
			}
			off := int(leaves[1]-1) * 512
			data[off+3], data[off+4] = 0, 0
		}
		return data
	}

	res := runDB(t, build(false))
	requireClean(t, res)
	got := byLocator(t, res)
	for i := 301; i <= 305; i++ {
		r, ok := got[fmt.Sprintf("history_items:%d", i)]
		if !ok || rawMap(r)["visitless"] != true || rawMap(r)["join_undecided"] != nil || r.Time != nil {
			t.Errorf("intact control, item %d: %+v", i, r.Payload)
		}
	}

	res = runDB(t, build(true))
	if res.Err != nil {
		t.Fatalf("err %v", res.Err)
	}
	got = byLocator(t, res)
	for i := int64(1); i <= 300; i++ {
		r, ok := got[fmt.Sprintf("history_items:%d", i)]
		switch {
		case !lost[i] && ok:
			t.Errorf("item %d has a visit on an intact page: no item record expected", i)
		case lost[i] && (!ok || rawMap(r)["visitless"] != nil || !slices.Equal(list(rawMap(r), "join_undecided"), []string{"history_visits"})):
			t.Errorf("item %d lost its visit page: want a join_undecided record, got %v %+v", i, ok, r.Payload)
		}
	}
	for i := 301; i <= 305; i++ {
		r, ok := got[fmt.Sprintf("history_items:%d", i)]
		raw := rawMap(r)
		if !ok || raw["visitless"] != nil || !slices.Equal(list(raw, "join_undecided"), []string{"history_visits"}) || r.Time != nil {
			t.Errorf("damaged visits, item %d: %+v", i, r.Payload)
		}
	}
}

// The rows of a visit whose item is gone are warnings, never records with an invented URL (4F-R4).
func TestDanglingHistoryItemIsAWarning(t *testing.T) {
	d := newDB(1024)
	d.item(1, "https://a.example/", int64(1))
	d.visit(1, 1, tVisit, "ok")
	d.visit(2, 99, tVisit, "dangling")
	res := runDB(t, d.b.Bytes())
	if res.Err != nil || len(res.Records) != 1 || len(res.Warnings) != 1 {
		t.Fatalf("err %v, %d records, warnings %+v", res.Err, len(res.Records), res.Warnings)
	}
	w := res.Warnings[0]
	if w.Locator != "sqlite:table=history_visits;rowid=2" || !strings.Contains(w.Reason, "history_item") {
		t.Errorf("warning %+v", w)
	}
	if res.Notes["unmapped_rows.history_visits"] != "1" {
		t.Errorf("notes %v", res.Notes)
	}
}

// 4F-R30: Get by rowid is a join on id only when the id cell is the rowid.
func TestItemWhoseIdIsNotItsRowidIsAWarning(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	items := b.CreateTable("history_items", "create table history_items(id integer, url text, visit_count integer)")
	items.Insert(1, int64(7), "https://a.example/", int64(1))
	visits := b.CreateTable("history_visits", visitsDDL)
	visits.Insert(1, int64(1), int64(1), tVisit, "t", nil, nil, nil)
	res := runDB(t, b.Bytes())
	visitRecs := 0
	for _, r := range res.Records {
		if strings.Contains(r.Locator, "history_visits") {
			visitRecs++
		}
	}
	if res.Err != nil || visitRecs != 0 || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Reason, "history_item") {
		t.Errorf("err %v, %d visit records, warnings %+v", res.Err, visitRecs, res.Warnings)
	}
}

// A cell of the wrong storage class is unknown and named, never zero or absent.
func TestWrongStorageClassIsUnknownNotZero(t *testing.T) {
	d := newDB(1024)
	d.item(1, "https://a.example/", 2.5) // REAL visit_count
	d.item(2, "https://b.example/", int64(1))
	d.visit(1, 1, "yesterday", "text time")
	d.visits.Insert(2, int64(2), int64(1), tVisit, "bad bool", int64(2), nil, nil)
	d.visits.Insert(3, int64(3), "x", tVisit, "text item key", nil, nil, nil)
	d.visits.Insert(4, int64(4), int64(2), tVisit+1, int64(5), nil, nil, 1.5) // integer title, REAL redirect
	res := runDB(t, d.b.Bytes())
	if res.Err != nil || len(res.Warnings) != 1 || res.Warnings[0].Locator != "sqlite:table=history_visits;rowid=3" || !strings.Contains(res.Warnings[0].Reason, "history_item") {
		t.Fatalf("err %v warnings %+v: want one warning for the TEXT item key", res.Err, res.Warnings)
	}
	got := byLocator(t, res)

	v1 := got["history_visits:1"]
	if v1.Time != nil || rawMap(v1)["visit_time"] != nil || rawMap(v1)["visit_time_status"] != nil || sub(rawMap(v1), "unknown_columns")["visit_time"] != "type-mismatch" {
		t.Errorf("text visit_time: time %v raw %v", v1.Time, rawMap(v1))
	}
	if v1.Payload["url_visit_count"] != nil || sub(rawMap(v1), "joined_unknown_columns")["history_items.visit_count"] != "type-mismatch" {
		t.Errorf("REAL visit_count: %v", v1.Payload)
	}
	v2 := got["history_visits:2"]
	if v2.Payload["load_successful"] != nil || sub(rawMap(v2), "unknown_columns")["load_successful"] != "not-boolean" || v2.Time == nil {
		t.Errorf("load_successful 2: %v", v2.Payload)
	}
	v4 := got["history_visits:4"]
	unk := sub(rawMap(v4), "unknown_columns")
	if v4.Payload["title"] != nil || unk["title"] != "type-mismatch" || unk["redirect_destination"] != "type-mismatch" || v4.Payload["redirect"] != nil || v4.Body != "https://b.example/" {
		t.Errorf("integer title, REAL redirect: %v body %q", v4.Payload, v4.Body)
	}
}

func TestNegativeVisitCountIsNamedNotStated(t *testing.T) {
	d := newDB(1024)
	d.item(1, "https://a.example/", int64(-3))
	d.item(2, "https://b.example/", int64(-4))
	d.visit(1, 1, tVisit, "t")
	res := runDB(t, d.b.Bytes())
	requireClean(t, res)
	got := byLocator(t, res)
	v := got["history_visits:1"]
	if v.Payload["url_visit_count"] != nil || sub(rawMap(v), "joined_unknown_columns")["history_items.visit_count"] != "negative" {
		t.Errorf("visit: %v", v.Payload)
	}
	i := got["history_items:2"]
	if i.Payload["url_visit_count"] != nil || sub(rawMap(i), "unknown_columns")["visit_count"] != "negative" || rawMap(i)["visitless"] != true {
		t.Errorf("visitless item: %v", i.Payload)
	}
}

func TestREALVisitCountOnAVisitlessItemIsUnknown(t *testing.T) {
	d := newDB(1024)
	d.item(1, "https://a.example/", 2.5)
	res := runDB(t, d.b.Bytes())
	requireClean(t, res)
	i := byLocator(t, res)["history_items:1"]
	if i.Payload["url_visit_count"] != nil || sub(rawMap(i), "unknown_columns")["visit_count"] != "type-mismatch" {
		t.Errorf("%v", i.Payload)
	}
}

// A url that is not text is no URL: the row is a warning, never a record with an invented or empty URL.
func TestNonTextUrlIsAWarning(t *testing.T) {
	d := newDB(1024)
	d.item(1, int64(7), int64(1))
	d.item(2, int64(8), int64(0))
	d.item(3, []byte{1, 2}, int64(0))
	d.item(4, "", int64(0))
	d.visit(1, 1, tVisit, "t")
	res := runDB(t, d.b.Bytes())
	if res.Err != nil || len(res.Records) != 0 || len(res.Warnings) != 4 {
		t.Fatalf("err %v, %d records, warnings %+v", res.Err, len(res.Records), res.Warnings)
	}
	for _, w := range res.Warnings {
		if !strings.Contains(w.Reason, "url") {
			t.Errorf("warning %+v", w)
		}
	}
	if res.Notes["unmapped_rows.history_items"] != "3" || res.Notes["unmapped_rows.history_visits"] != "1" {
		t.Errorf("notes %v", res.Notes)
	}
}

func TestInvalidUTF8TombstoneURLIsCleanedAndFlagged(t *testing.T) {
	d := newDB(1024)
	d.tomb.Insert(1, int64(1), tVisit-100, tVisit-10, "https://t.example/\xff", int64(1))
	d.tomb.Insert(2, int64(2), tVisit-100, tVisit-10, nil, nil)
	d.tomb.Insert(3, int64(3), tVisit-100, tVisit-10, "", nil)
	d.tomb.Insert(4, int64(4), tVisit-100, tVisit-10, []byte("blob"), nil)
	res := runDB(t, d.b.Bytes())
	requireClean(t, res)
	if len(res.Records) != 1 || res.Notes["tombstones_without_url"] != "3" {
		t.Fatalf("%d records, notes %v", len(res.Records), res.Notes)
	}
	r := res.Records[0]
	raw := rawMap(r)
	if r.Payload["url"] != "https://t.example/�" || !slices.Equal(list(raw, "text_flags"), []string{"url"}) ||
		raw["url_raw_b64"] != base64.StdEncoding.EncodeToString([]byte("https://t.example/\xff")) || !r.Deleted || r.Time != nil {
		t.Errorf("%v deleted %v", r.Payload, r.Deleted)
	}
}

// Text above the caps is cut and the cut is said: the field in text_flags, the uncut size, the body cut per part.
func TestOversizedTextIsCutAndSaid(t *testing.T) {
	url := "https://big.example/" + strings.Repeat("u", 2<<20+100)
	title := strings.Repeat("t", 300<<10)
	d := newDB(4096)
	d.item(1, url, int64(1))
	d.visit(1, 1, tVisit, title)
	res := runDB(t, d.b.Bytes())
	requireClean(t, res)
	v := byLocator(t, res)["history_visits:1"]
	raw := rawMap(v)
	gotURL, _ := v.Payload["url"].(string)
	gotTitle, _ := v.Payload["title"].(string)
	if len(gotURL) != 2<<20 || len(gotTitle) != 256<<10 || raw["url_truncated"] != true || raw["url_total_bytes"] != int64(len(url)) ||
		raw["title_total_bytes"] != int64(len(title)) || !slices.Equal(list(raw, "text_flags"), []string{"title", "url"}) {
		t.Errorf("url %d title %d raw %v", len(gotURL), len(gotTitle), raw)
	}
	wantBody := 64<<10 + 1 + 64<<10
	if len(v.Body) != wantBody || raw["body_truncated"] != true || raw["body_total_bytes"] != int64(2<<20+1+256<<10) {
		t.Errorf("body %d (want %d), raw %v %v", len(v.Body), wantBody, raw["body_truncated"], raw["body_total_bytes"])
	}
}
