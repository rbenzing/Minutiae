package chromium_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	dlDDL = "create table downloads(id integer primary key, guid varchar, current_path longvarchar, target_path longvarchar, start_time integer, received_bytes integer, total_bytes integer, state integer, danger_type integer, interrupt_reason integer, hash blob, end_time integer, opened integer, last_access_time integer, transient integer, referrer varchar, site_url varchar, tab_url varchar, tab_referrer_url varchar, http_method varchar, mime_type varchar, original_mime_type varchar)"
	// no primary key on (id, chain_index): the in-memory tests build files a real History cannot hold
	chainDDL = "create table downloads_url_chains(id integer not null, chain_index integer not null, url longvarchar not null)"
)

var dlCols = []string{"id", "guid", "current_path", "target_path", "start_time", "received_bytes", "total_bytes", "state", "danger_type", "interrupt_reason", "hash", "end_time", "opened", "last_access_time", "transient", "referrer", "site_url", "tab_url", "tab_referrer_url", "http_method", "mime_type", "original_mime_type"}

// insertDL adds a download row: defaults for a complete download, over overrides the named columns (nil is NULL).
func insertDL(tb *sqlitetest.Table, id int64, over map[string]any) {
	row := map[string]any{
		"id": id, "guid": "G", "current_path": "/d/f", "target_path": "/d/f", "start_time": tMem, "received_bytes": int64(10),
		"total_bytes": int64(10), "state": int64(1), "danger_type": int64(0), "interrupt_reason": int64(0), "end_time": tMem + 1e6,
		"opened": int64(0), "last_access_time": int64(0), "transient": int64(0),
	}
	for k, v := range over {
		row[k] = v
	}
	vals := make([]any, len(dlCols))
	for i, c := range dlCols {
		vals[i] = row[c]
	}
	tb.Insert(id, vals...)
}

// dlDB is a database of urls, visits, downloads and downloads_url_chains.
func dlDB(page int) (*sqlitetest.Builder, *sqlitetest.Table, *sqlitetest.Table) {
	b, _, _ := memDB(page)
	return b, b.CreateTable("downloads", dlDDL), b.CreateTable("downloads_url_chains", chainDDL)
}

func payloadOf(t *testing.T, res parsertest.Result, rowid int64) (records.Record, map[string]any, map[string]any) {
	t.Helper()
	r := recFor(t, res, "downloads", rowid)
	return r, r.Payload, asMap(r.Payload["raw"])
}

func chainList(p map[string]any) []string {
	l, _ := p["url_chain"].([]any)
	out := make([]string, len(l))
	for i, v := range l {
		out[i], _ = v.(string)
	}
	return out
}

func hasRec(res parsertest.Result, table string, rowid int64) bool {
	loc := fmt.Sprintf("sqlite:table=%s;rowid=%d", table, rowid)
	for _, r := range res.Records {
		if r.Locator == loc {
			return true
		}
	}
	return false
}

// runStrict runs the parser over an in-memory database under the strict harness: a refused record fails the test.
func runStrict(t *testing.T, db []byte) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	bundle := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": db}}.Bundle(h, remoteChrome)
	p := chromium.Parser{}
	return h.Run(p, bundle.Input(h, p, false))
}

// 4F-R11: the chain follows chain_index, not rowid; gaps and a missing chain are stated, and a chainless download
// states no url.
func TestURLChainFollowsChainIndexNotRowid(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "current")
	b := fx.Bundle(h, remoteChrome)
	p := chromium.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	noViolations(t, res)

	_, pl, raw := payloadOf(t, res, 1) // the index-1 row has the lower rowid
	if got := chainList(pl); !slices.Equal(got, []string{"https://example.com/dl?id=1", "https://cdn.example.com/report.pdf"}) {
		t.Errorf("url_chain %v, want index 0 first", got)
	}
	if pl["url"] != "https://cdn.example.com/report.pdf" {
		t.Errorf("url %v, want the last (index 1) entry", pl["url"])
	}
	if _, ok := raw["url_chain_state"]; ok {
		t.Errorf("a complete chain states url_chain_state %v", raw["url_chain_state"])
	}
	_, pl, raw = payloadOf(t, res, 5)
	if raw["url_chain_state"] != "gaps" || len(chainList(pl)) != 2 || pl["url"] != "https://example.com/gaps-2" {
		t.Errorf("gaps download: state %v chain %v url %v", raw["url_chain_state"], pl["url_chain"], pl["url"])
	}
	_, pl, raw = payloadOf(t, res, 3)
	if raw["url_chain_state"] != "no-chain-rows" {
		t.Errorf("chainless download state %v, want no-chain-rows", raw["url_chain_state"])
	}
	for _, k := range []string{"url", "url_chain"} {
		if _, ok := pl[k]; ok {
			t.Errorf("chainless download states %s", k)
		}
	}
}

// 4F-R11: a chain_index that is not an integer makes the chain undecided for that download: no chain and no url.
func TestNonIntegerChainIndexLeavesTheChainUnknown(t *testing.T) {
	b, d, c := dlDB(1024)
	insertDL(d, 1, nil)
	c.Insert(1, int64(1), int64(0), "https://a.example/0")
	c.Insert(2, int64(1), "one", "https://a.example/1")
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	_, pl, raw := payloadOf(t, res, 1)
	if raw["url_chain_state"] != "unknown-index" {
		t.Errorf("state %v, want unknown-index", raw["url_chain_state"])
	}
	for _, k := range []string{"url", "url_chain"} {
		if _, ok := pl[k]; ok {
			t.Errorf("an undecided chain states %s", k)
		}
	}
}

// 4F-R11: a download with neither a usable target_path nor a chain URL is a counted warning, never a record.
func TestDownloadWithoutTargetOrChainIsAWarning(t *testing.T) {
	b, d, c := dlDB(1024)
	insertDL(d, 1, map[string]any{"target_path": "", "current_path": "/d/only-current"})
	insertDL(d, 2, map[string]any{"target_path": "/d/ok"})
	insertDL(d, 3, map[string]any{"target_path": "", "current_path": ""})
	c.Insert(1, int64(3), int64(0), "https://a.example/3")
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if hasRec(res, "downloads", 1) || !hasRec(res, "downloads", 2) || !hasRec(res, "downloads", 3) {
		t.Errorf("records %+v: only download 1 (no target, no chain) must be missing", res.Records)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Locator != "sqlite:table=downloads;rowid=1" || !strings.Contains(res.Warnings[0].Reason, "target_path") {
		t.Errorf("warnings %+v, want one for sqlite:table=downloads;rowid=1 naming target_path", res.Warnings)
	}
	if res.Notes["unmapped_rows.downloads"] != "1" {
		t.Errorf("note unmapped_rows.downloads = %q, want 1", res.Notes["unmapped_rows.downloads"])
	}
	// the chain alone is enough: a download whose target_path is empty keeps its url
	if _, pl, _ := payloadOf(t, res, 3); pl["url"] != "https://a.example/3" {
		t.Errorf("download 3 payload %v, want the chain url", pl)
	}
}

// 4F-R8 and 4F-R33: Chromium writes 0 for "size not recorded"; no total_bytes is stated, the stored 0 stays in raw,
// and received_bytes (a real count) states its 0.
func TestTotalBytesZeroIsAbsent(t *testing.T) {
	b, d, _ := dlDB(1024)
	insertDL(d, 1, map[string]any{"state": int64(4), "interrupt_reason": int64(22), "received_bytes": int64(1024), "total_bytes": int64(0)})
	insertDL(d, 2, map[string]any{"received_bytes": int64(0), "total_bytes": int64(-5)})
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	_, pl, raw := payloadOf(t, res, 1)
	if _, ok := pl["total_bytes"]; ok || raw["total_bytes"] != int64(0) || pl["received_bytes"] != int64(1024) {
		t.Errorf("payload %v raw %v, want no total_bytes, raw.total_bytes 0, received_bytes 1024", pl, raw)
	}
	if pl["state"] != "interrupted" || pl["interrupt_reason"] != "network_disconnected" {
		t.Errorf("state %v interrupt_reason %v", pl["state"], pl["interrupt_reason"])
	}
	_, pl, raw = payloadOf(t, res, 2)
	if _, ok := pl["total_bytes"]; ok || asMap(raw["unknown_columns"])["total_bytes"] != "negative" || pl["received_bytes"] != int64(0) {
		t.Errorf("payload %v raw %v, want a negative total_bytes flagged and received_bytes 0", pl, raw)
	}
}

// The enum columns: a stored value no table lists is "unknown" (the integer stays in raw), bug_140687 is a real
// state and is not swallowed, and 32 (obsolete in the source) is unknown, not a guessed name.
func TestUnlistedEnumValuesAreUnknownAndTheIntegerIsKept(t *testing.T) {
	b, d, _ := dlDB(1024)
	insertDL(d, 1, map[string]any{"state": int64(3)})
	insertDL(d, 2, map[string]any{"state": int64(9), "danger_type": int64(99), "interrupt_reason": int64(32)})
	insertDL(d, 3, map[string]any{"state": "complete", "danger_type": nil})
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	_, pl, raw := payloadOf(t, res, 1)
	if pl["state"] != "bug_140687" || raw["state"] != int64(3) {
		t.Errorf("state %v raw %v, want bug_140687 and 3", pl["state"], raw["state"])
	}
	_, pl, raw = payloadOf(t, res, 2)
	if pl["state"] != "unknown" || pl["danger_type"] != "unknown" || pl["interrupt_reason"] != "unknown" ||
		raw["state"] != int64(9) || raw["danger_type"] != int64(99) || raw["interrupt_reason"] != int64(32) {
		t.Errorf("payload %v raw %v, want unknown names and the stored integers", pl, raw)
	}
	_, pl, raw = payloadOf(t, res, 3)
	if _, ok := pl["state"]; ok || asMap(raw["unknown_columns"])["state"] != "type-mismatch" {
		t.Errorf("payload %v raw %v, want no state and a type-mismatch", pl, raw)
	}
	if _, ok := pl["danger_type"]; ok {
		t.Errorf("a NULL danger_type is stated as %v", pl["danger_type"])
	}
}

// 4F-R26: a hostile chain of 1000 rows of 64 KiB is cut at 2 MiB of URL bytes; the record passes the strict harness
// (the body stays under 1 MiB) and says what was cut.
func TestHugeChainIsCappedAndAcceptedByTheStrictHarness(t *testing.T) {
	const n, size = 1000, 64 << 10
	b, d, c := dlDB(4096)
	insertDL(d, 1, nil)
	for i := range n {
		u := fmt.Sprintf("https://h%04d.example/", i)
		c.Insert(int64(i+1), int64(1), int64(i), u+strings.Repeat("a", size-len(u)))
	}
	res := runStrict(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(res.Records) != 1 {
		t.Fatalf("%d records, want 1", len(res.Records))
	}
	r, pl, raw := payloadOf(t, res, 1)
	if raw["url_chain_bytes_truncated"] != true || raw["url_chain_total"] != int64(n) || raw["url_chain_bytes_total"] != int64(n*size) {
		t.Errorf("raw %v, want url_chain_bytes_truncated, url_chain_total %d, url_chain_bytes_total %d", raw, n, n*size)
	}
	if _, ok := raw["url_chain_truncated"]; ok {
		t.Error("the 1000-entry cap was not exceeded but url_chain_truncated is set")
	}
	kept := 0
	for _, u := range chainList(pl) {
		kept += len(u)
	}
	if kept == 0 || kept > 2<<20 {
		t.Errorf("kept chain is %d bytes, want 1..2 MiB", kept)
	}
	if got := chainList(pl); len(got) != 32 || !strings.HasPrefix(got[0], "https://h0000.example/") {
		t.Errorf("kept %d entries, want the first 32 in index order", len(got))
	}
	if !strings.HasPrefix(fmt.Sprint(pl["url"]), "https://h0999.example/") {
		t.Errorf("url %.30v..., want the last entry of the whole chain", pl["url"])
	}
	if len(r.Body) > 1<<20 || raw["body_truncated"] != true {
		t.Errorf("body is %d bytes, body_truncated %v: want at most 1 MiB and flagged", len(r.Body), raw["body_truncated"])
	}
	if want := int64(len(strings.Join(append([]string{"/d/f"}, chainList(pl)...), "\n"))); raw["body_total_bytes"] != want {
		t.Errorf("body_total_bytes %v, want %d, the uncut length of the kept parts", raw["body_total_bytes"], want)
	}
}

// 4F-R11 and 4F-R15: a target path over the 4096-byte contract cap is cut on a rune boundary and flagged, and the
// record passes the strict harness.
func TestTargetPathOverTheContractCapIsCutAndFlagged(t *testing.T) {
	b, d, _ := dlDB(1024)
	long := "/d/" + strings.Repeat("x", 4092) + strings.Repeat("\xc3\xa9", 454) // 5003 bytes; a 2-byte rune straddles byte 4096
	insertDL(d, 1, map[string]any{"target_path": long, "current_path": "/d/short"})
	res := runStrict(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	_, pl, raw := payloadOf(t, res, 1)
	tp, _ := pl["target_path"].(string)
	if len(tp) != 4095 || !strings.HasPrefix(long, tp) {
		t.Errorf("target_path is %d bytes, want the 4095-byte prefix that ends on a rune boundary", len(tp))
	}
	flags, _ := raw["text_flags"].([]any)
	if !slices.Contains(flags, any("target_path")) || raw["target_path_total_bytes"] != int64(len(long)) {
		t.Errorf("text_flags %v target_path_total_bytes %v, want target_path flagged and %d", flags, raw["target_path_total_bytes"], len(long))
	}
}

// 4F-R11: a chain entry that states no URL text (NULL, empty or a blob) leaves the chain unknown for that download:
// no chain and no url, the download itself is still recorded from its target path.
func TestChainEntryWithoutURLTextLeavesTheChainUnknown(t *testing.T) {
	b, _, _ := memDB(1024)
	d := b.CreateTable("downloads", dlDDL)
	c := b.CreateTable("downloads_url_chains", "create table downloads_url_chains(id integer, chain_index integer, url longvarchar)")
	for i, u := range []any{nil, "", []byte{1, 2}} {
		id := int64(i + 1)
		insertDL(d, id, nil)
		c.Insert(id*10, id, int64(0), "https://ok.example/first")
		c.Insert(id*10+1, id, int64(1), u)
	}
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	for id := int64(1); id <= 3; id++ {
		_, pl, raw := payloadOf(t, res, id)
		if raw["url_chain_state"] != "unknown-url" || pl["target_path"] != "/d/f" {
			t.Errorf("download %d: state %v target_path %v, want unknown-url and the target path", id, raw["url_chain_state"], pl["target_path"])
		}
		for _, k := range []string{"url", "url_chain"} {
			if _, ok := pl[k]; ok {
				t.Errorf("download %d states %s from a chain with an unreadable entry", id, k)
			}
		}
	}
}

// Cells of the wrong storage class are stated in unknown_columns and never read as values; a downloads.id that is not
// an integer leaves the chain join undecided.
func TestDownloadWrongClassesAreStatedAndANonIntegerIDLeavesTheChainUndecided(t *testing.T) {
	b, d, c := dlDB(1024)
	insertDL(d, 1, map[string]any{"referrer": int64(5), "tab_url": []byte{9}, "mime_type": int64(7)})
	c.Insert(1, int64(1), int64(0), "https://a.example/1")
	res := runMem(t, b.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	_, pl, raw := payloadOf(t, res, 1)
	unk := asMap(raw["unknown_columns"])
	for _, k := range []string{"referrer", "tab_url", "mime_type"} {
		if unk[k] != "type-mismatch" {
			t.Errorf("unknown_columns.%s = %v, want type-mismatch", k, unk[k])
		}
	}
	for _, k := range []string{"referrer", "tab_url", "mime"} {
		if _, ok := pl[k]; ok {
			t.Errorf("payload states %s from a cell of the wrong class", k)
		}
	}

	b2 := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	b2.CreateTable("urls", urlsDDL)
	b2.CreateTable("visits", visitsDDL)
	d2 := b2.CreateTable("downloads", strings.Replace(dlDDL, "id integer primary key", "id text", 1))
	b2.CreateTable("downloads_url_chains", chainDDL)
	insertDL(d2, 1, map[string]any{"id": "abc"})
	res = runMem(t, b2.Bytes())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	_, pl, raw = payloadOf(t, res, 1)
	if j, _ := raw["join_undecided"].([]any); len(j) != 1 || j[0] != "downloads_url_chains" || asMap(raw["unknown_columns"])["id"] != "type-mismatch" {
		t.Errorf("raw %v, want join_undecided [downloads_url_chains] and unknown_columns.id type-mismatch", raw)
	}
	if _, ok := raw["url_chain_state"]; ok || pl["url"] != nil {
		t.Errorf("an undecided chain states %v / url %v", raw["url_chain_state"], pl["url"])
	}
}
