package chromium

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

// Cell storage classes of the fuzzed rows.
const (
	cNull byte = iota
	cInt
	cFloat
	cText
	cBlob
	numKinds
)

type cell struct {
	kind byte
	i    int64
	f    float64
	b    []byte
}

// fuzzRow is a decoded row of any table: the mappings see only the parse.Row interface, so their behaviour on any
// storage class in any column is fuzzed here without a database. Its damage flags are driven by the fuzz bytes.
type fuzzRow struct {
	table string
	rowid int64
	cells []cell
	flags sqlitedb.RowFlags
}

// Flags are the decoder's damage flags of the row (rowrun.DamageFlags reads them).
func (r fuzzRow) Flags() sqlitedb.RowFlags { return r.flags }

// damage is the row_flags list the flags must give.
func damage(f sqlitedb.RowFlags) []any {
	var out []any
	if f&sqlitedb.FlagLengthMismatch != 0 {
		out = append(out, "length_mismatch")
	}
	if f&sqlitedb.FlagKeyRange != 0 {
		out = append(out, "key_range")
	}
	if f&sqlitedb.FlagExtraValues != 0 {
		out = append(out, "extra_values")
	}
	return out
}

const allFlags = sqlitedb.FlagLengthMismatch | sqlitedb.FlagKeyRange | sqlitedb.FlagExtraValues

func (r fuzzRow) Table() string        { return r.table }
func (r fuzzRow) Rowid() (int64, bool) { return r.rowid, true }
func (r fuzzRow) IsNull(c int) bool    { return r.cells[c].kind == cNull }
func (r fuzzRow) Int(c int) (int64, bool) {
	return r.cells[c].i, r.cells[c].kind == cInt
}

func (r fuzzRow) Float(c int) (float64, bool) {
	return r.cells[c].f, r.cells[c].kind == cFloat
}

func (r fuzzRow) Text(c int) ([]byte, bool) {
	return r.cells[c].b, r.cells[c].kind == cText
}

func (r fuzzRow) Blob(c int) ([]byte, bool) {
	return r.cells[c].b, r.cells[c].kind == cBlob
}

func (fuzzRow) Range() (records.Range, parse.FileRole) {
	return records.Range{Offset: 0, Length: 10}, "db"
}

func (r fuzzRow) Locator() (string, bool) {
	return "sqlite:table=" + r.table + ";rowid=" + strconv.FormatInt(r.rowid, 10), true
}

// Join answers of the fake context.
const (
	joinFound   = iota // Get finds the urls row; Match finds no visit and the chain rows
	joinHit            // as joinFound, but Match finds a visit
	joinMissing        // Get: a clean miss; Match: an undecidable key
	joinError          // any other error
	joinBudget         // the budget error
	numJoinModes
)

// The tables' columns in the order the fuzzed rows hold them: the specs of the mapping, urls led by its id and the
// chain table as the parser reads it.
func tableColumns(table string) []string {
	switch table {
	case "urls":
		return append([]string{"id"}, names(urlsSpecs())...)
	case "visits":
		return names(visitsSpecs())
	case "keyword_search_terms":
		return names(searchSpecs())
	case "downloads":
		return names(downloadSpecs())
	}
	return []string{"id", "chain_index", "url"}
}

func colOf(table, name string) int {
	for i, n := range tableColumns(table) {
		if n == name {
			return i
		}
	}
	return -1
}

// fakeCtx answers its joins by mode. url is the urls row Get returns, chain the rows Match returns for a download.
type fakeCtx struct {
	in    *parse.Input
	mode  int
	url   fuzzRow
	chain []fuzzRow
}

func (c fakeCtx) Input() *parse.Input { return c.in }
func (fakeCtx) Note(_, _ string)      {}

func (fakeCtx) Column(table, col string) int { return colOf(table, col) }

func (c fakeCtx) Get(string, int64) (parse.Row, bool, error) {
	switch c.mode {
	case joinMissing:
		return nil, false, nil
	case joinError:
		return nil, false, errors.New("corrupt page")
	case joinBudget:
		return nil, false, parse.ErrBudget
	}
	return c.url, true, nil
}

func (c fakeCtx) Match(table, _ string, _ parse.JoinKey) ([]parse.Row, error) {
	switch c.mode {
	case joinMissing:
		return nil, sqlitedb.ErrKeyUndecidable
	case joinError:
		return nil, errors.New("index failed")
	case joinBudget:
		return nil, parse.ErrBudget
	}
	if table == "visits" {
		if c.mode == joinHit {
			return []parse.Row{fuzzRow{table: "visits", rowid: 1, cells: make([]cell, len(tableColumns("visits")))}}, nil
		}
		return nil, nil
	}
	rows := make([]parse.Row, len(c.chain))
	for i, r := range c.chain {
		rows[i] = r
	}
	return rows, nil
}

const logicalChrome = "android:/data/data/com.android.chrome/app_chrome/Default/History"

func inputOf() *parse.Input {
	a := parse.Artifact{ID: "a1", Size: 100, Logical: logicalChrome, Source: parse.SourceInfo{RemotePath: "/p/History"}}
	return &parse.Input{Primary: a, Artifacts: map[string]parse.Artifact{"db": a}}
}

func ints(table string, set map[string]cell) []cell {
	cs := make([]cell, len(tableColumns(table)))
	for i, n := range tableColumns(table) {
		cs[i] = cell{kind: cInt}
		if c, ok := set[n]; ok {
			cs[i] = c
		}
	}
	return cs
}

func txt(s string) cell { return cell{kind: cText, b: []byte(s)} }

func num(v int64) cell { return cell{kind: cInt, i: v} }

const tWebkit = int64(13327408800000000) // 2023-05-01T10:00:00Z

func goodURL() fuzzRow {
	return fuzzRow{table: "urls", rowid: 1, cells: ints("urls", map[string]cell{
		"id": num(1), "url": txt("https://a.example/"), "title": txt("Title"), "visit_count": num(3), "typed_count": num(1),
		"last_visit_time": num(tWebkit), "hidden": num(0),
	})}
}

func goodVisit() fuzzRow {
	return fuzzRow{table: "visits", rowid: 1, cells: ints("visits", map[string]cell{
		"url": num(1), "visit_time": num(tWebkit), "transition": num(0), "from_visit": num(0), "visit_duration": num(5e6),
	})}
}

func goodSearch() fuzzRow {
	return fuzzRow{table: "keyword_search_terms", rowid: 1, cells: ints("keyword_search_terms", map[string]cell{
		"keyword_id": num(2), "url_id": num(1), "term": txt("a term"), "normalized_term": txt("a term"),
	})}
}

func goodDownload() fuzzRow {
	return fuzzRow{table: "downloads", rowid: 1, cells: ints("downloads", map[string]cell{
		"id": num(1), "target_path": txt("/d/f.pdf"), "start_time": num(tWebkit), "received_bytes": num(10), "total_bytes": num(10),
		"state": num(1), "end_time": num(tWebkit + 1e6),
	})}
}

func goodChain() []fuzzRow {
	mk := func(rowid, idx int64, u string) fuzzRow {
		return fuzzRow{table: "downloads_url_chains", rowid: rowid, cells: []cell{num(1), num(idx), txt(u)}}
	}
	return []fuzzRow{mk(1, 0, "https://a.example/0"), mk(2, 1, "https://a.example/1")}
}

func mapRow(row, url fuzzRow, chain []fuzzRow, mode int) ([]records.Record, error) {
	m := fakeCtx{in: inputOf(), mode: mode, url: url, chain: chain}
	switch row.table {
	case "urls":
		return mapURL(m, row)
	case "visits":
		return mapVisit(m, row)
	case "keyword_search_terms":
		return mapSearch(m, row)
	}
	return mapDownload(m, row)
}

func rawOf(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

// usable is a text cell the mappings may state: text, not empty.
func usable(c cell) bool { return c.kind == cText && len(c.b) > 0 }

// isInt says a cell holds an integer, the only class a time or count may be read from.
func isInt(c cell) bool { return c.kind == cInt }

// checkMapped holds a mapping to its contract on any row: no panic, and either a typed error (a RowError, or the
// budget error returned to the runner), no record by design, or one record that validates, carries its place, is
// never recovered or deleted, and states no URL, title, term, path, time or count that the cells do not say.
func checkMapped(t testing.TB, row, url fuzzRow, chain []fuzzRow, mode int) {
	t.Helper()
	recs, err := mapRow(row, url, chain, mode)
	if err != nil {
		var re *rowrun.RowError
		if !errors.As(err, &re) && !errors.Is(err, parse.ErrBudget) {
			t.Fatalf("untyped error %v", err)
		}
		if len(recs) != 0 {
			t.Fatalf("error %v with %d records", err, len(recs))
		}
		return
	}
	if mode == joinBudget {
		switch {
		case row.table == "visits" && isInt(row.cells[colOf("visits", "url")]),
			row.table == "keyword_search_terms" && usable(row.cells[colOf(row.table, "term")]) && isInt(row.cells[colOf(row.table, "url_id")]),
			row.table == "downloads" && isInt(row.cells[colOf("downloads", "id")]),
			row.table == "urls" && row.cells[0].kind == cInt && row.cells[0].i == row.rowid:
			t.Fatalf("%s: a budget error of a join was not returned", row.table)
		}
	}
	if len(recs) > 1 {
		t.Fatalf("%d records", len(recs))
	}
	clean := func(c cell, limit int) string { return common.CleanText(c.b, limit).Text }
	for _, r := range recs {
		if r.Locator != "sqlite:table="+row.table+";rowid=1" || r.ArtifactID != "a1" || r.SourcePath != "/p/History" || r.Range == nil {
			t.Fatalf("record lost its place: %+v", r)
		}
		var validate func(map[string]any) error
		switch r.Type {
		case web.VisitType:
			validate = web.ValidateVisit
		case web.SearchType:
			validate = web.ValidateSearch
		case web.DownloadType:
			validate = web.ValidateDownload
		default:
			t.Fatalf("record type %q", r.Type)
		}
		if err := validate(r.Payload); err != nil {
			t.Fatalf("payload refused: %v", err)
		}
		if _, err := json.Marshal(r.Payload); err != nil {
			t.Fatalf("payload does not marshal: %v", err)
		}
		if r.Deleted || r.Recovery != "" || r.Confidence != nil || r.TimeEnd != nil {
			t.Fatalf("a recovery or deletion fact on a live row: %+v", r)
		}
		raw := rawOf(r)
		got := raw["row_flags"]
		if row.flags&allFlags == 0 {
			if got != nil {
				t.Errorf("row_flags %v on an undamaged row", got)
			}
		} else if !reflect.DeepEqual(got, damage(row.flags)) {
			t.Errorf("row_flags %v from flags %b", got, row.flags)
		}
		switch row.table {
		case "urls":
			u := row.cells[colOf("urls", "url")]
			if !usable(u) || r.Payload["url"] != clean(u, maxURL) || r.Payload["visit_id"] != nil {
				t.Errorf("urls record %+v from url cell %+v", r, u)
			}
			decided := mode == joinFound
			if _, visitless := raw["visitless"]; visitless != decided || (raw["join_undecided"] != nil) == decided {
				t.Errorf("mode %d: raw %v", mode, raw)
			}
			if r.Time != nil && !isInt(row.cells[colOf("urls", "last_visit_time")]) {
				t.Errorf("a time from a last_visit_time cell of kind %d", row.cells[colOf("urls", "last_visit_time")].kind)
			}
			checkURLFacts(t, r.Payload, row)
		case "visits":
			u := url.cells[colOf("urls", "url")]
			if !usable(u) || r.Payload["url"] != clean(u, maxURL) || r.Payload["visit_id"] != "1" {
				t.Errorf("visit record %+v from urls url cell %+v", r, u)
			}
			if tc := url.cells[colOf("urls", "title")]; tc.kind != cText {
				if _, has := r.Payload["title"]; has {
					t.Errorf("a title from a cell of kind %d", tc.kind)
				}
			} else if title, has := r.Payload["title"]; has && title != clean(tc, maxText) {
				t.Errorf("title %v from %q", title, tc.b)
			}
			if r.Time != nil && !isInt(row.cells[colOf("visits", "visit_time")]) {
				t.Errorf("a time from a visit_time cell of kind %d", row.cells[colOf("visits", "visit_time")].kind)
			}
			if _, has := r.Payload["transition"]; has && !isInt(row.cells[colOf("visits", "transition")]) {
				t.Errorf("a transition from a cell of kind %d", row.cells[colOf("visits", "transition")].kind)
			}
			if d, has := r.Payload["duration_s"]; has {
				dc := row.cells[colOf("visits", "visit_duration")]
				if !isInt(dc) || dc.i <= 0 || d != float64(dc.i)/1e6 {
					t.Errorf("duration_s %v from %+v", d, dc)
				}
			}
			checkURLFacts(t, r.Payload, url)
		case "keyword_search_terms":
			tc := row.cells[colOf(row.table, "term")]
			if !usable(tc) || r.Payload["term"] != clean(tc, maxText) || r.Body != r.Payload["term"] {
				t.Errorf("search record %+v from term cell %+v", r, tc)
			}
			if _, has := r.Payload["engine"]; has {
				t.Errorf("a search states an engine: %v", r.Payload["engine"])
			}
			stated := r.Payload["url"] != nil
			if stated && (!usable(url.cells[colOf("urls", "url")]) || r.Payload["url"] != clean(url.cells[colOf("urls", "url")], maxURL) || (mode != joinFound && mode != joinHit)) {
				t.Errorf("search url %v from urls row %+v in mode %d", r.Payload["url"], url.cells, mode)
			}
			if (r.Time != nil) != (r.Payload["time_source"] != nil) || (r.Time != nil && !isInt(url.cells[colOf("urls", "last_visit_time")])) {
				t.Errorf("time %v and time_source %v from last_visit_time %+v", r.Time, r.Payload["time_source"], url.cells[colOf("urls", "last_visit_time")])
			}
		default:
			checkDownload(t, r, row, chain, clean)
		}
	}
	if len(recs) == 0 {
		switch row.table {
		case "visits", "keyword_search_terms", "downloads":
			t.Fatalf("a %s row mapped to no record and no error", row.table)
		case "urls":
			if mode != joinHit && usable(row.cells[colOf("urls", "url")]) && row.cells[0].kind == cInt && row.cells[0].i == row.rowid {
				t.Fatalf("a urls row with a usable url and no visit known to exist mapped to nothing (mode %d)", mode)
			}
		}
	}
}

// checkURLFacts holds the url_visit_count, typed_count and hidden of a payload to the urls row's cells.
func checkURLFacts(t testing.TB, payload map[string]any, urlRow fuzzRow) {
	t.Helper()
	for key, col := range map[string]string{"url_visit_count": "visit_count", "typed_count": "typed_count"} {
		if n, has := payload[key]; has {
			if c := urlRow.cells[colOf("urls", col)]; !isInt(c) || c.i < 0 || n != c.i {
				t.Errorf("%s %v from %+v", key, n, c)
			}
		}
	}
	if h, has := payload["hidden"]; has {
		if c := urlRow.cells[colOf("urls", "hidden")]; !isInt(c) || (c.i != 0 && c.i != 1) || h != (c.i == 1) {
			t.Errorf("hidden %v from %+v", h, c)
		}
	}
}

// checkDownload holds a download record to its cells and chain rows.
func checkDownload(t testing.TB, r records.Record, row fuzzRow, chain []fuzzRow, clean func(cell, int) string) {
	t.Helper()
	tp := row.cells[colOf("downloads", "target_path")]
	if v, has := r.Payload["target_path"]; has && (!usable(tp) || v != clean(tp, maxPath)) {
		t.Errorf("target_path %v from %+v", v, tp)
	}
	if u, has := r.Payload["url"]; has {
		found := false
		for _, c := range chain {
			if cu := c.cells[2]; usable(cu) && u == clean(cu, maxURL) {
				found = true
			}
		}
		if !found {
			t.Errorf("download url %v is the text of no chain row", u)
		}
	}
	if r.Time != nil && !isInt(row.cells[colOf("downloads", "start_time")]) {
		t.Errorf("a time from a start_time cell of kind %d", row.cells[colOf("downloads", "start_time")].kind)
	}
	for _, nt := range r.Times {
		col := "end_time"
		if nt.Kind == "accessed" {
			col = "last_access_time"
		} else if nt.Kind != "ended" {
			t.Errorf("Times kind %q", nt.Kind)
		}
		if c := row.cells[colOf("downloads", col)]; !isInt(c) {
			t.Errorf("a %s from a %s cell of kind %d", nt.Kind, col, c.kind)
		}
	}
	if v, has := r.Payload["total_bytes"]; has {
		if c := row.cells[colOf("downloads", "total_bytes")]; !isInt(c) || c.i <= 0 || v != c.i {
			t.Errorf("total_bytes %v from %+v", v, c)
		}
	}
	if v, has := r.Payload["received_bytes"]; has {
		if c := row.cells[colOf("downloads", "received_bytes")]; !isInt(c) || c.i < 0 || v != c.i {
			t.Errorf("received_bytes %v from %+v", v, c)
		}
	}
}

// cellsFrom builds a row of n columns from fuzz bytes: kinds picks each storage class, nums supplies 8 bytes a
// column for the integer and float (any bit pattern, so NaN and the infinities occur), texts is split at NUL bytes.
func cellsFrom(n int, kinds, nums, texts []byte) []cell {
	var parts [][]byte
	start := 0
	for i, b := range texts {
		if b == 0 {
			parts = append(parts, texts[start:i])
			start = i + 1
		}
	}
	parts = append(parts, texts[start:])
	cs := make([]cell, n)
	for i := range cs {
		if len(kinds) > 0 {
			cs[i].kind = kinds[i%len(kinds)] % numKinds
		}
		var w [8]byte
		if len(nums) > 0 {
			for j := range w {
				w[j] = nums[(i*8+j)%len(nums)]
			}
		}
		u := binary.LittleEndian.Uint64(w[:])
		cs[i].i, cs[i].f = int64(u), math.Float64frombits(u)
		cs[i].b = parts[i%len(parts)]
	}
	return cs
}

// A damaged row still maps, and says so: raw.row_flags lists each flag the decoder raised, on every mapping that
// reads a row (urls, visits, searches and downloads).
func TestDamagedRowsMapAndSayWhatWasDamaged(t *testing.T) {
	for _, row := range []fuzzRow{goodURL(), goodVisit(), goodSearch(), goodDownload()} {
		for _, tc := range []struct {
			f    sqlitedb.RowFlags
			want []any
		}{
			{sqlitedb.FlagLengthMismatch, []any{"length_mismatch"}},
			{sqlitedb.FlagKeyRange, []any{"key_range"}},
			{sqlitedb.FlagExtraValues, []any{"extra_values"}},
			{allFlags, []any{"length_mismatch", "key_range", "extra_values"}},
		} {
			row.flags = tc.f
			recs, err := mapRow(row, goodURL(), goodChain(), joinFound)
			if err != nil || len(recs) != 1 {
				t.Fatalf("%s %b: err %v, %d records", row.table, tc.f, err, len(recs))
			}
			if got := rawOf(recs[0])["row_flags"]; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: row_flags %v, want %v", row.table, got, tc.want)
			}
		}
		row.flags = 0
		recs, _ := mapRow(row, goodURL(), goodChain(), joinFound)
		if rawOf(recs[0])["row_flags"] != nil {
			t.Errorf("%s: row_flags on an undamaged row", row.table)
		}
	}
}

func TestMapRowsGoodRowsMapAndHostileCellsAreContained(t *testing.T) {
	for _, row := range []fuzzRow{goodURL(), goodVisit(), goodSearch(), goodDownload()} {
		if recs, err := mapRow(row, goodURL(), goodChain(), joinFound); err != nil || len(recs) != 1 {
			t.Fatalf("%s: err %v, records %+v", row.table, err, recs)
		}
	}
	visit, _ := mapRow(goodVisit(), goodURL(), nil, joinFound)
	if visit[0].Time == nil || visit[0].Payload["url_visit_count"] != int64(3) || visit[0].Payload["title"] != "Title" || visit[0].Payload["url"] != "https://a.example/" {
		t.Errorf("good visit: %+v", visit[0])
	}
	search, _ := mapRow(goodSearch(), goodURL(), nil, joinFound)
	if search[0].Time == nil || search[0].Payload["time_source"] != "urls.last_visit_time" || search[0].Payload["term"] != "a term" {
		t.Errorf("good search: %+v", search[0])
	}
	dl, _ := mapRow(goodDownload(), fuzzRow{}, goodChain(), joinFound)
	if dl[0].Payload["url"] != "https://a.example/1" || len(dl[0].Times) != 1 || dl[0].Payload["target_path"] != "/d/f.pdf" {
		t.Errorf("good download: %+v", dl[0])
	}
	for mode := range numJoinModes {
		for _, row := range []fuzzRow{goodURL(), goodVisit(), goodSearch(), goodDownload()} {
			checkMapped(t, row, goodURL(), goodChain(), mode)
			for _, k := range []byte{cNull, cInt, cFloat, cText, cBlob} {
				for col := range row.cells {
					r := fuzzRow{table: row.table, rowid: 1, cells: append([]cell(nil), row.cells...)}
					r.cells[col] = cell{kind: k, i: math.MinInt64, f: math.NaN(), b: []byte{0xff, 0xfe, 0}}
					checkMapped(t, r, goodURL(), goodChain(), mode)
					u := goodURL()
					u.cells[col%len(u.cells)] = cell{kind: k, i: -1, f: math.Inf(1), b: []byte{0xff}}
					checkMapped(t, row, u, goodChain(), mode)
					ch := goodChain()
					ch[1].cells[col%3] = cell{kind: k, i: math.MaxInt64, f: math.NaN(), b: []byte{0xff}}
					checkMapped(t, row, goodURL(), ch, mode)
				}
			}
		}
	}
}

// FuzzMapChromiumRows fuzzes the four mappings on decoded column values directly: every column of the row, of the
// urls row a visit, search or download names, and of the download's chain rows takes any storage class and value,
// REAL columns any bit pattern, the damage flags any combination, and the joins answer by mode.
func FuzzMapChromiumRows(f *testing.F) {
	nan := []byte{0, 0, 0, 0, 0, 0, 0xf8, 0x7f}
	inf := []byte{0, 0, 0, 0, 0, 0, 0xf0, 0x7f}
	one := []byte{1, 0, 0, 0, 0, 0, 0, 0}
	f.Add(byte(0), byte(0), byte(0), []byte{cInt, cText, cInt, cInt}, one, []byte("https://a.example/\x00t"))
	f.Add(byte(1), byte(0xc0), byte(9), []byte{cInt, cInt, cText, cInt, cNull}, nan, []byte("\xff\xfe\x00 12"))
	f.Add(byte(2), byte(0xc0), byte(7), []byte{cInt, cInt, cText, cText}, inf, []byte("https://gone.example/"))
	f.Add(byte(3), byte(0xc1), byte(3), []byte{cInt, cText, cInt, cInt, cInt}, one, []byte("/d/f\x00https://c.example/"))
	f.Add(byte(3), byte(0xc0), byte(0), []byte{cFloat, cBlob, cNull}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("u"))
	f.Fuzz(func(t *testing.T, which, mode, fl byte, kinds, nums, texts []byte) {
		table := [4]string{"urls", "visits", "keyword_search_terms", "downloads"}[which%4]
		flags := sqlitedb.RowFlags(fl) & allFlags
		rev := make([]byte, len(kinds))
		for i, k := range kinds {
			rev[len(kinds)-1-i] = k
		}
		url := fuzzRow{table: "urls", rowid: 1, cells: cellsFrom(len(tableColumns("urls")), rev, nums, texts)}
		if mode&0x80 != 0 {
			url.cells[0] = cell{kind: cInt, i: 1} // the urls row's id is the key the other rows name
		}
		row := fuzzRow{table: table, rowid: 1, cells: cellsFrom(len(tableColumns(table)), kinds, nums, texts), flags: flags}
		if mode&0x40 != 0 {
			switch table {
			case "urls":
				row.cells[0] = cell{kind: cInt, i: 1}
			case "visits":
				row.cells[colOf(table, "url")] = cell{kind: cInt, i: 1}
			case "keyword_search_terms":
				row.cells[colOf(table, "url_id")] = cell{kind: cInt, i: 1}
			case "downloads":
				row.cells[colOf(table, "id")] = cell{kind: cInt, i: 1}
			}
		}
		chain := make([]fuzzRow, 0, 3)
		for i := range int(mode>>4&0x3) + 1 {
			c := fuzzRow{table: "downloads_url_chains", rowid: int64(i + 1), cells: cellsFrom(3, rev[min(i, len(rev)):], nums[min(i, len(nums)):], texts[min(i, len(texts)):])}
			chain = append(chain, c)
		}
		checkMapped(t, row, url, chain, int(mode&0xf)%numJoinModes)
	})
}
