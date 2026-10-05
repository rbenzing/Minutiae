package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

func ptr[T any](v T) *T { return &v }

// verbatimURL is a URL a normalizer would change in six ways: scheme and host
// case, the default port, dot segments, a duplicated query key and the fragment.
const verbatimURL = "HTTPS://User@Example.TEST:443/a/../b/?x=1&x=2&X=3#Frag%20ment now"

func minimalVisit() web.Visit {
	return web.Visit{URL: "https://example.test/", Browser: "unknown-chromium"}
}

func fullVisit() web.Visit {
	return web.Visit{
		URL: verbatimURL, Browser: "chrome", Title: "Example  page", VisitID: "12", Profile: "Default", Transition: "typed",
		TransitionQualifiers: []string{"from_address_bar", "client_redirect"},
		Referrer:             &web.Referrer{VisitID: "11", URL: "HTTP://Example.TEST/prev#x"},
		RedirectSource:       "10", RedirectDestination: "13",
		DurationS: ptr(2.5), URLVisitCount: ptr(int64(7)), TypedCount: ptr(int64(0)), StatusCode: ptr(int64(200)),
		Hidden: ptr(false), LoadSuccessful: ptr(true),
		Raw:      map[string]any{"transition": int64(805306372), "nested": map[string]any{"a": []any{"x", int64(2), 1.5}}},
		Recovery: map[string]any{"relation": "absent-from-live", "via": "wal", "wal": map[string]any{"frame": int64(3), "committed": true}},
		Snapshot: map[string]any{"name": "com.apple.snap", "xid": int64(12)},
		Deleted:  map[string]any{"source": "history_tombstones"},
	}
}

func minimalSearch() web.Search { return web.Search{Term: "x"} }

func fullSearch() web.Search {
	return web.Search{
		Term: "Cats  and DOGS", NormalizedTerm: "cats and dogs", Engine: "Example Search", EngineID: "2", URL: "HTTPS://Example.TEST/search?q=Cats+and+DOGS#top",
		VisitID: "12", TimeSource: "urls.last_visit_time", Browser: "edge",
		Raw:      map[string]any{"url_id": int64(4), "nested": map[string]any{"a": []any{"x"}}},
		Recovery: map[string]any{"relation": "superseded-version"},
		Snapshot: map[string]any{"name": "snap", "xid": int64(2)},
		Deleted:  map[string]any{"source": "x"},
	}
}

func minimalDownload() web.Download { return web.Download{URL: "https://example.test/f"} }

func fullDownload() web.Download {
	return web.Download{
		TargetPath: "/sdcard/Download/Report FINAL.PDF", CurrentPath: "/sdcard/Download/Report FINAL.PDF.crdownload",
		URL: "HTTPS://Example.TEST/f?a=1#frag", Referrer: "HTTPS://Example.TEST/", TabURL: "HTTPS://Example.TEST/tab", Mime: "application/pdf",
		State: "complete", DangerType: "not_dangerous", InterruptReason: "none", Browser: "brave",
		URLChain:   []string{"HTTP://Example.TEST/redirect", "HTTPS://Example.TEST/f?a=1#frag"},
		TotalBytes: ptr(int64(1234)), ReceivedBytes: ptr(int64(0)), Opened: ptr(false),
		Raw:      map[string]any{"id": int64(9), "nested": map[string]any{"a": []any{"x"}}},
		Recovery: map[string]any{"relation": "uncommitted"},
		Snapshot: map[string]any{"name": "snap", "xid": int64(2)},
		Deleted:  map[string]any{"source": "x"},
	}
}

// kind bundles what the tests need to treat the three types alike.
type kind struct {
	name     string
	typ      string
	validate func(map[string]any) error
	valid    map[string]map[string]any // named valid payloads (builder output)
	invalid  map[string]any            // a payload the validator refuses
}

func kinds(t *testing.T) []kind {
	t.Helper()
	return []kind{
		{
			"visit", web.VisitType, web.ValidateVisit,
			map[string]map[string]any{"minimal": minimalVisit().Payload(), "full": fullVisit().Payload()},
			map[string]any{"url": "https://example.test/"},
		},
		{
			"search", web.SearchType, web.ValidateSearch,
			map[string]map[string]any{"minimal": minimalSearch().Payload(), "full": fullSearch().Payload()},
			map[string]any{"term": ""},
		},
		{
			"download", web.DownloadType, web.ValidateDownload,
			map[string]map[string]any{"minimal": minimalDownload().Payload(), "full": fullDownload().Payload()},
			map[string]any{"mime": "x"},
		},
	}
}

func TestPayloadValidatorsAcceptParserOutput(t *testing.T) {
	cs := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, cs, "history.db", make([]byte, 1<<16))
	w, err := records.NewWriter(cs, records.Parser{Name: "web-test", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, k := range kinds(t) {
		for name, p := range k.valid {
			if err := k.validate(p); err != nil {
				t.Errorf("%s %s: Validate(builder output) = %v", k.name, name, err)
			}
			if err := w.Add(ctx, records.Record{Type: k.typ, ArtifactID: a.ID, Summary: k.name + " " + name, Payload: p}); err != nil {
				t.Errorf("the writer refused the %s %s record: %v", k.name, name, err)
			}
			n++
		}
		// the same writer refuses a violation with the typed error
		if err := w.Add(ctx, records.Record{Type: k.typ, ArtifactID: a.ID, Summary: "bad", Payload: k.invalid}); !errors.Is(err, records.ErrInvalidPayload) {
			t.Errorf("Add(invalid %s) = %v, want ErrInvalidPayload", k.name, err)
		}
	}
	if res, err := w.End(ctx); err != nil || res.Records != int64(n) || res.Rejected != 3 {
		t.Fatalf("End = %+v, %v", res, err)
	}
}

func TestWebRegisteredValidators(t *testing.T) {
	infos := map[string]records.TypeInfo{}
	for _, ti := range records.Types() {
		infos[ti.Name] = ti
	}
	for _, name := range []string{web.VisitType, web.SearchType, web.DownloadType} {
		ti, ok := infos[name]
		if !ok {
			t.Errorf("%s is not a registered type", name)
			continue
		}
		if !ti.HasValidator {
			t.Errorf("%s has no validator although its package is imported", name)
		}
		if ti.PayloadVersion != web.PayloadVersion {
			t.Errorf("%s: registered payload version %d, package says %d", name, ti.PayloadVersion, web.PayloadVersion)
		}
	}
	if web.VisitType != "web_visit" || web.SearchType != "web_search" || web.DownloadType != "download" || web.PayloadVersion != 1 {
		t.Error("a type name or the payload version constant changed")
	}
}

type mut = func(p map[string]any)

func clone(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPayloadValidatorsRejectViolations: one row per F10 rule of the three types.
// Every error names the path (and the reason), and none repeats a value.
func TestPayloadValidatorsRejectViolations(t *testing.T) {
	const marker = "SECRET-VALUE-4417"
	ks := map[string]kind{}
	for _, k := range kinds(t) {
		ks[k.name] = k
	}
	type row struct {
		kind, name string
		fn         mut
		want       string
	}
	var rows []row
	add := func(kind, name string, fn mut, want string) { rows = append(rows, row{kind, name, fn, want}) }
	set := func(k string, v any) mut { return func(p map[string]any) { p[k] = v } }
	del := func(k string) mut { return func(p map[string]any) { delete(p, k) } }
	sub := func(obj, k string, v any) mut {
		return func(p map[string]any) { p[obj].(map[string]any)[k] = v }
	}
	str := strings.Repeat("a", 1<<12+1)

	// ---- web_visit
	add("visit", "missing url", del("url"), "payload.url: required field is missing")
	add("visit", "missing browser", del("browser"), "payload.browser: required field is missing")
	add("visit", "empty url", set("url", ""), "payload.url: must not be empty")
	add("visit", "url type", set("url", int64(4)), "payload.url")
	add("visit", "browser enum", set("browser", "firefox"), "payload.browser: not one of chrome,chromium,webview,edge,brave,opera,safari,unknown-chromium")
	add("visit", "browser case", set("browser", "Chrome"), "payload.browser")
	add("visit", "browser type", set("browser", true), "payload.browser")
	add("visit", "transition enum", set("transition", "click"), "payload.transition: not one of")
	add("visit", "transition type", set("transition", int64(1)), "payload.transition")
	for f, v := range map[string]any{
		"title": int64(1), "visit_id": 1.5, "profile": true, "transition_qualifiers": "x", "referrer": "x", "duration_s": "x",
		"url_visit_count": "x", "typed_count": 1.5, "status_code": "200", "hidden": "yes", "load_successful": int64(1), "raw": "x",
		"recovery": "x", "snapshot": "x", "deleted": true, "redirect": []any{},
	} {
		add("visit", "wrong type of "+f, set(f, v), "payload."+f)
	}
	add("visit", "visit_id empty", set("visit_id", ""), "payload.visit_id")
	add("visit", "visit_id float", set("visit_id", 12.0), "payload.visit_id")
	add("visit", "visit_id json.Number exponent", set("visit_id", json.Number("1e3")), "payload.visit_id")
	add("visit", "visit_id array", set("visit_id", []any{}), "payload.visit_id")
	add("visit", "qualifiers element type", set("transition_qualifiers", []any{"a", int64(1)}), "payload.transition_qualifiers[1]")
	add("visit", "qualifiers 10,001", set("transition_qualifiers", make([]any, 10001)), "payload.transition_qualifiers")
	add("visit", "referrer.visit_id type", sub("referrer", "visit_id", true), "payload.referrer.visit_id")
	add("visit", "referrer.url type", sub("referrer", "url", int64(1)), "payload.referrer.url")
	add("visit", "referrer.url empty", sub("referrer", "url", ""), "payload.referrer.url")
	add("visit", "redirect.source_visit_id type", sub("redirect", "source_visit_id", 1.5), "payload.redirect.source_visit_id")
	add("visit", "redirect.destination_visit_id type", sub("redirect", "destination_visit_id", []any{}), "payload.redirect.destination_visit_id")
	add("visit", "duration negative", set("duration_s", float64(-1)), "payload.duration_s: below the minimum 0")
	add("visit", "duration NaN", set("duration_s", math.NaN()), "payload.duration_s")
	add("visit", "duration Inf", set("duration_s", math.Inf(1)), "payload.duration_s")
	add("visit", "duration json.Number NaN", set("duration_s", json.Number("NaN")), "payload.duration_s")
	add("visit", "url_visit_count negative", set("url_visit_count", int64(-1)), "payload.url_visit_count: below the minimum 0")
	add("visit", "typed_count negative", set("typed_count", int64(-1)), "payload.typed_count: below the minimum 0")
	add("visit", "url_visit_count exponent", set("url_visit_count", json.Number("1e3")), "payload.url_visit_count")
	add("visit", "status_code decimal", set("status_code", json.Number("200.0")), "payload.status_code")
	add("visit", "status_code -0", set("status_code", json.Number("-0")), "payload.status_code")
	add("visit", "raw depth 5", set("raw", map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": 1}}}}), "payload.raw")
	add("visit", "raw array 10,001", set("raw", map[string]any{"a": make([]any, 10001)}), "payload.raw")
	add("visit", "raw unsupported value", set("raw", map[string]any{"a": struct{}{}}), "payload.raw")

	// ---- web_search
	add("search", "missing term", del("term"), "payload.term: required field is missing")
	add("search", "empty term", set("term", ""), "payload.term: must not be empty")
	add("search", "term type", set("term", int64(1)), "payload.term")
	for f, v := range map[string]any{
		"normalized_term": int64(1), "engine": true, "engine_id": 1.5, "url": int64(1), "visit_id": true, "time_source": int64(1),
		"browser": int64(1), "raw": "x", "recovery": "x", "snapshot": "x", "deleted": "x",
	} {
		add("search", "wrong type of "+f, set(f, v), "payload."+f)
	}
	add("search", "browser enum", set("browser", "firefox"), "payload.browser: not one of")
	add("search", "engine_id empty", set("engine_id", ""), "payload.engine_id")
	add("search", "visit_id empty", set("visit_id", ""), "payload.visit_id")
	add("search", "url empty", set("url", ""), "payload.url: must not be empty")

	// ---- download
	add("download", "neither target_path nor url", func(p map[string]any) { delete(p, "target_path"); delete(p, "url") }, "at least one of target_path,url must be present and not empty")
	add("download", "both empty", func(p map[string]any) { p["target_path"] = ""; p["url"] = "" }, "payload.target_path")
	add("download", "target_path type", set("target_path", int64(1)), "payload.target_path")
	add("download", "target_path empty", set("target_path", ""), "payload.target_path: must not be empty")
	add("download", "target_path too long", set("target_path", str), "payload.target_path: longer than 4096 bytes")
	add("download", "current_path empty", set("current_path", ""), "payload.current_path: must not be empty")
	add("download", "current_path too long", set("current_path", str), "payload.current_path: longer than 4096 bytes")
	add("download", "url empty", set("url", ""), "payload.url: must not be empty")
	for f, v := range map[string]any{
		"current_path": int64(1), "url": int64(1), "referrer": true, "tab_url": int64(1), "mime": int64(1), "state": true, "danger_type": int64(1),
		"interrupt_reason": 1.5, "browser": int64(1), "url_chain": "x", "total_bytes": "1", "received_bytes": 1.5, "opened": "no", "raw": "x",
		"recovery": "x", "snapshot": "x", "deleted": "x",
	} {
		add("download", "wrong type of "+f, set(f, v), "payload."+f)
	}
	add("download", "browser enum", set("browser", "firefox"), "payload.browser: not one of")
	add("download", "url_chain element type", set("url_chain", []any{"a", int64(1)}), "payload.url_chain[1]")
	add("download", "url_chain 10,001", set("url_chain", make([]any, 10001)), "payload.url_chain")
	add("download", "total_bytes negative", set("total_bytes", int64(-1)), "payload.total_bytes: below the minimum 0")
	add("download", "received_bytes negative", set("received_bytes", int64(-1)), "payload.received_bytes: below the minimum 0")
	add("download", "total_bytes exponent", set("total_bytes", json.Number("1e3")), "payload.total_bytes")
	add("download", "received_bytes decimal", set("received_bytes", json.Number("2.0")), "payload.received_bytes")
	add("download", "total_bytes past int64", set("total_bytes", json.Number("9223372036854775808")), "payload.total_bytes")
	add("download", "total_bytes uint64 past int64", set("total_bytes", uint64(math.MaxInt64)+1), "payload.total_bytes")

	// ---- common provenance rules, for every type
	for _, k := range []string{"visit", "search", "download"} {
		add(k, "deleted without source", set("deleted", map[string]any{}), "payload.deleted.source")
		add(k, "deleted empty source", set("deleted", map[string]any{"source": ""}), "payload.deleted.source")
		add(k, "deleted source type", set("deleted", map[string]any{"source": int64(1)}), "payload.deleted.source")
		add(k, "recovery relation", set("recovery", map[string]any{"relation": "bogus"}), "payload.recovery.relation")
		add(k, "snapshot without name", set("snapshot", map[string]any{"xid": int64(1)}), "payload.snapshot.name")
		add(k, "snapshot negative xid", set("snapshot", map[string]any{"name": "s", "xid": int64(-1)}), "payload.snapshot.xid")
		add(k, "snapshot decimal xid", set("snapshot", map[string]any{"name": "s", "xid": json.Number("1.0")}), "payload.snapshot.xid")
		add(k, "empty payload", func(p map[string]any) { clear(p) }, "payload")
	}

	for _, r := range rows {
		k := ks[r.kind]
		t.Run(r.kind+"/"+r.name, func(t *testing.T) {
			p := clone(t, k.valid["full"])
			// clone turns numbers into float64 and removes nothing; keep the original types where a row needs them
			p = restoreInts(p)
			r.fn(p)
			err := k.validate(p)
			if err == nil {
				t.Fatalf("an invalid payload was accepted: %v", p)
			}
			if !strings.Contains(err.Error(), r.want) {
				t.Errorf("error %q does not contain %q", err, r.want)
			}
		})
	}

	// no error repeats a payload value
	for _, k := range kinds(t) {
		for _, f := range []string{"url", "term", "target_path", "browser", "title", "mime", "state"} {
			p := clone(t, k.valid["full"])
			if _, ok := p[f]; !ok {
				continue
			}
			p[f] = marker
			p["raw"] = "x"
			if err := k.validate(p); err != nil && strings.Contains(err.Error(), marker) {
				t.Errorf("%s %s: error repeats a payload value: %v", k.name, f, err)
			}
		}
		if err := k.validate(map[string]any{"direction": marker}); err == nil || strings.Contains(err.Error(), marker) {
			t.Errorf("%s: %v", k.name, err)
		}
	}
	// accepted boundaries
	boundary := map[string]map[string]any{
		"visit":    {"url": "u", "browser": "safari", "duration_s": 0.0, "url_visit_count": int64(0), "typed_count": int64(0), "status_code": int64(-1), "visit_id": int64(0)},
		"search":   {"term": " ", "engine_id": int64(-3), "visit_id": "a"},
		"download": {"target_path": strings.Repeat("a", 4096), "current_path": strings.Repeat("b", 4096), "total_bytes": int64(math.MaxInt64), "received_bytes": int64(0)},
	}
	for name, p := range boundary {
		if err := ks[name].validate(p); err != nil {
			t.Errorf("%s boundary payload refused: %v", name, err)
		}
	}
}

// restoreInts turns the float64 numbers json.Unmarshal made back into int64 where
// they are whole, so the rows start from the types the builder writes.
func restoreInts(v map[string]any) map[string]any {
	var fix func(any) any
	fix = func(x any) any {
		switch y := x.(type) {
		case float64:
			if y == math.Trunc(y) && math.Abs(y) < 1<<53 {
				return int64(y)
			}
		case map[string]any:
			for k, e := range y {
				y[k] = fix(e)
			}
		case []any:
			for i, e := range y {
				y[i] = fix(e)
			}
		}
		return x
	}
	return fix(v).(map[string]any)
}

// TestWebVisitIDAcceptsStringAndInt: ids are strings in the typed structs, but a
// source that stores numeric ids may be written either way and both validate and
// decode to the same canonical string.
func TestWebVisitIDAcceptsStringAndInt(t *testing.T) {
	for _, id := range []any{"12", "abc-1", int64(12), 12, json.Number("12"), uint64(12), json.Number("-7")} {
		for name, build := range map[string]func(any) (map[string]any, func(map[string]any) error){
			"visit": func(id any) (map[string]any, func(map[string]any) error) {
				p := minimalVisit().Payload()
				p["visit_id"] = id
				return p, web.ValidateVisit
			},
			"referrer": func(id any) (map[string]any, func(map[string]any) error) {
				p := minimalVisit().Payload()
				p["referrer"] = map[string]any{"visit_id": id}
				return p, web.ValidateVisit
			},
			"redirect src": func(id any) (map[string]any, func(map[string]any) error) {
				p := minimalVisit().Payload()
				p["redirect"] = map[string]any{"source_visit_id": id}
				return p, web.ValidateVisit
			},
			"redirect dest": func(id any) (map[string]any, func(map[string]any) error) {
				p := minimalVisit().Payload()
				p["redirect"] = map[string]any{"destination_visit_id": id}
				return p, web.ValidateVisit
			},
			"search visit": func(id any) (map[string]any, func(map[string]any) error) {
				p := minimalSearch().Payload()
				p["visit_id"] = id
				return p, web.ValidateSearch
			},
			"search engine": func(id any) (map[string]any, func(map[string]any) error) {
				p := minimalSearch().Payload()
				p["engine_id"] = id
				return p, web.ValidateSearch
			},
		} {
			p, validate := build(id)
			if err := validate(p); err != nil {
				t.Errorf("%s: id %#v refused: %v", name, id, err)
			}
		}
	}
	// decode: a numeric id becomes its canonical decimal string, a string stays
	for in, want := range map[string]string{`12`: "12", `"12"`: "12", `"007"`: "007", `-7`: "-7", `0`: "0", `9223372036854775807`: "9223372036854775807", `"a b"`: "a b"} {
		v, err := web.DecodeVisit(1, []byte(`{"url":"u","browser":"chrome","visit_id":`+in+`,"referrer":{"visit_id":`+in+`},"redirect":{"source_visit_id":`+in+`,"destination_visit_id":`+in+`}}`))
		if err != nil {
			t.Errorf("DecodeVisit(%s): %v", in, err)
			continue
		}
		if v.VisitID != want || v.Referrer == nil || v.Referrer.VisitID != want || v.RedirectSource != want || v.RedirectDestination != want {
			t.Errorf("DecodeVisit(%s) = %+v, want every id %q", in, v, want)
		}
		s, err := web.DecodeSearch(1, []byte(`{"term":"t","visit_id":`+in+`,"engine_id":`+in+`}`))
		if err != nil || s.VisitID != want || s.EngineID != want {
			t.Errorf("DecodeSearch(%s) = %+v, %v", in, s, err)
		}
	}
	// the builder writes the string as given: "007" stays "007"
	if got := (web.Visit{URL: "u", Browser: "chrome", VisitID: "007"}).Payload()["visit_id"]; got != "007" {
		t.Errorf("the builder changed the id: %#v", got)
	}
	// a decoded numeric id builds a payload that validates
	v, err := web.DecodeVisit(1, []byte(`{"url":"u","browser":"chrome","visit_id":12}`))
	if err != nil || web.ValidateVisit(v.Payload()) != nil {
		t.Errorf("decoded numeric id: %v / %v", err, web.ValidateVisit(v.Payload()))
	}
}

func TestDownloadAtLeastOneOfTargetPathOrURL(t *testing.T) {
	ok := map[string]web.Download{
		"url only":             {URL: "https://example.test/f"},
		"target path only":     {TargetPath: "/sdcard/Download/a.bin"},
		"both":                 {URL: "u", TargetPath: "/a"},
		"current path and url": {URL: "u", CurrentPath: "/a.crdownload"},
	}
	for name, d := range ok {
		if err := web.ValidateDownload(d.Payload()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]web.Download{
		"nothing":                   {},
		"only a current path":       {CurrentPath: "/a.crdownload"},
		"only a referrer":           {Referrer: "https://example.test/"},
		"only a tab url and a mime": {TabURL: "https://example.test/tab", Mime: "application/pdf"},
		"only a chain":              {URLChain: []string{"https://example.test/f"}},
		"only sizes":                {TotalBytes: ptr(int64(1)), ReceivedBytes: ptr(int64(1))},
	}
	for name, d := range bad {
		err := web.ValidateDownload(d.Payload())
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "at least one of target_path,url must be present and not empty") {
			t.Errorf("%s: error %q does not name the rule", name, err)
		}
	}
	for name, p := range map[string]map[string]any{
		"nil payload":            nil,
		"empty strings":          {"target_path": "", "url": ""},
		"null values":            {"target_path": nil, "url": nil},
		"url of the wrong type":  {"url": int64(1), "target_path": "/a"},
		"empty url, good target": {"url": "", "target_path": "/a"},
	} {
		if err := web.ValidateDownload(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestDownloadPathAndSizeAreBoundedAndValidated: the local path is a bounded
// non-empty string; sizes are integers from 0 to MaxInt64 (a total of 0 is a
// legal "unknown" in the sources, so no relation between the two is imposed).
func TestDownloadPathAndSizeAreBoundedAndValidated(t *testing.T) {
	long := strings.Repeat("p", 4097)
	for _, d := range []web.Download{
		{URL: "u", TargetPath: long},
		{URL: "u", CurrentPath: long},
		{URL: "u", TotalBytes: ptr(int64(-1))},
		{URL: "u", ReceivedBytes: ptr(int64(-9))},
	} {
		if err := web.ValidateDownload(d.Payload()); err == nil {
			t.Errorf("accepted %+v", d)
		}
	}
	for _, d := range []web.Download{
		{URL: "u", TargetPath: long[:4096], CurrentPath: long[:4096]},
		{URL: "u", TotalBytes: ptr(int64(0)), ReceivedBytes: ptr(int64(5))},
		{URL: "u", TotalBytes: ptr(int64(math.MaxInt64)), ReceivedBytes: ptr(int64(math.MaxInt64))},
	} {
		if err := web.ValidateDownload(d.Payload()); err != nil {
			t.Errorf("refused %+v: %v", d, err)
		}
	}
}

// TestWebURLsAreStoredVerbatim: no builder, validator or decoder normalizes a URL
// (case, default port, dot segments, query order, fragment, whitespace, userinfo,
// a very long one); a normalized form can only be an additional field.
func TestWebURLsAreStoredVerbatim(t *testing.T) {
	urls := []string{
		verbatimURL, "http://example.test", "HTTP://EXAMPLE.TEST/", "https://example.test:443/", "https://example.test/./a/../b",
		"https://example.test/?b=2&a=1&b=1", "https://example.test/#", "https://example.test/#a b", " https://example.test/ ", "https://example.test/\t",
		"https://ex\u00e4mple.test/\u00fc?q=\u00f6#\u00e9", "https://xn--exmple-cua.test/", "javascript:alert(1)", "about:blank", "data:text/plain;base64,AAAA", "file:///C:/Users/a/b.txt",
		"chrome://history/", "content://media/external/file/12", "HTTPS://example.test/%7Efoo/%7efoo", "https://example.test/" + strings.Repeat("a", 5000),
		"not a url at all", "x",
	}
	for _, u := range urls {
		v := web.Visit{URL: u, Browser: "safari", Referrer: &web.Referrer{URL: u}}
		s := web.Search{Term: "t", URL: u}
		d := web.Download{URL: u, Referrer: u, TabURL: u, URLChain: []string{u, u + "#2"}}
		for name, p := range map[string]map[string]any{"visit": v.Payload(), "search": s.Payload(), "download": d.Payload()} {
			b, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "visit":
				got, err := web.DecodeVisit(1, b)
				if err != nil || got.URL != u || got.Referrer == nil || got.Referrer.URL != u {
					t.Errorf("visit url %q came back as %q (%v)", u, got.URL, err)
				}
				if body := web.VisitBody(got); !strings.Contains(body, u) {
					t.Errorf("the body of a visit does not hold the url %q verbatim", u)
				}
			case "search":
				got, err := web.DecodeSearch(1, b)
				if err != nil || got.URL != u {
					t.Errorf("search url %q came back as %q (%v)", u, got.URL, err)
				}
			case "download":
				got, err := web.DecodeDownload(1, b)
				if err != nil || got.URL != u || got.Referrer != u || got.TabURL != u || !reflect.DeepEqual(got.URLChain, []string{u, u + "#2"}) {
					t.Errorf("download url %q came back as %+v (%v)", u, got, err)
				}
			}
		}
	}
	// the builder has no field that would hold a rewritten URL, and writes none
	for _, p := range []map[string]any{fullVisit().Payload(), fullSearch().Payload(), fullDownload().Payload()} {
		for k := range p {
			if strings.Contains(k, "normalized_url") || strings.Contains(k, "canonical") {
				t.Errorf("unexpected normalized form field %q", k)
			}
		}
	}
}

func TestWebBuildersOmitAbsent(t *testing.T) {
	for name, tc := range map[string]struct {
		p    map[string]any
		keys []string
	}{
		"visit":                        {web.Visit{URL: "u", Browser: "chrome"}.Payload(), []string{"browser", "url"}},
		"search":                       {web.Search{Term: "t"}.Payload(), []string{"term"}},
		"download":                     {web.Download{URL: "u"}.Payload(), []string{"url"}},
		"visit with empty sub objects": {web.Visit{URL: "u", Browser: "chrome", Referrer: &web.Referrer{}, TransitionQualifiers: []string{}, Raw: nil}.Payload(), []string{"browser", "url"}},
		"download with empty chain":    {web.Download{URL: "u", URLChain: []string{}}.Payload(), []string{"url"}},
	} {
		var got []string
		for k := range tc.p {
			got = append(got, k)
		}
		if len(got) != len(tc.keys) {
			t.Errorf("%s: payload keys %v, want %v", name, got, tc.keys)
		}
		for _, k := range tc.keys {
			if _, ok := tc.p[k]; !ok {
				t.Errorf("%s: missing %s", name, k)
			}
		}
	}
	// a set pointer is written even when zero or false
	v := web.Visit{URL: "u", Browser: "chrome", DurationS: ptr(0.0), URLVisitCount: ptr(int64(0)), TypedCount: ptr(int64(0)), StatusCode: ptr(int64(0)), Hidden: ptr(false), LoadSuccessful: ptr(false)}
	p := v.Payload()
	for _, k := range []string{"duration_s", "url_visit_count", "typed_count", "status_code", "hidden", "load_successful"} {
		if _, ok := p[k]; !ok {
			t.Errorf("a set %s was not written", k)
		}
	}
	d := web.Download{URL: "u", TotalBytes: ptr(int64(0)), ReceivedBytes: ptr(int64(0)), Opened: ptr(false)}.Payload()
	for _, k := range []string{"total_bytes", "received_bytes", "opened"} {
		if _, ok := d[k]; !ok {
			t.Errorf("a set %s was not written", k)
		}
	}
	// a partly set referrer and redirect write only what is set
	pr := web.Visit{URL: "u", Browser: "chrome", Referrer: &web.Referrer{URL: "r"}, RedirectDestination: "9"}.Payload()
	if !reflect.DeepEqual(pr["referrer"], map[string]any{"url": "r"}) || !reflect.DeepEqual(pr["redirect"], map[string]any{"destination_visit_id": "9"}) {
		t.Errorf("referrer/redirect = %#v / %#v", pr["referrer"], pr["redirect"])
	}
	// a builder given a violation does not hide it
	if err := web.ValidateVisit(web.Visit{URL: "u", Browser: "chrome", DurationS: ptr(-1.0)}.Payload()); err == nil {
		t.Error("a negative duration was hidden by the builder")
	}
	if err := web.ValidateVisit(web.Visit{Browser: "chrome"}.Payload()); err == nil {
		t.Error("a visit without a url validated")
	}
	if err := web.ValidateSearch(web.Search{}.Payload()); err == nil {
		t.Error("a search without a term validated")
	}
	if err := web.ValidateDownload(web.Download{}.Payload()); err == nil {
		t.Error("an empty download validated")
	}
}

func canonical(v any) bool {
	switch x := v.(type) {
	case string, bool, int64, float64:
		return true
	case []any:
		for _, e := range x {
			if !canonical(e) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, e := range x {
			if !canonical(e) {
				return false
			}
		}
		return true
	}
	return false
}

func TestWebPayloadIsCanonicalTypes(t *testing.T) {
	for name, p := range map[string]map[string]any{
		"visit": minimalVisit().Payload(), "full visit": fullVisit().Payload(), "search": fullSearch().Payload(), "download": fullDownload().Payload(),
	} {
		if !canonical(p) {
			t.Errorf("%s payload holds a type outside string, bool, int64, float64, []any, map[string]any", name)
		}
	}
	// nothing in the payload aliases the typed value
	v := fullVisit()
	p := v.Payload()
	p["raw"].(map[string]any)["transition"] = "changed"
	p["recovery"].(map[string]any)["via"] = "changed"
	p["snapshot"].(map[string]any)["name"] = "changed"
	p["deleted"].(map[string]any)["source"] = "changed"
	p["transition_qualifiers"].([]any)[0] = "changed"
	p["referrer"].(map[string]any)["url"] = "changed"
	if v.Raw["transition"] != int64(805306372) || v.Recovery["via"] != "wal" || v.Snapshot["name"] != "com.apple.snap" || v.Deleted["source"] != "history_tombstones" ||
		v.TransitionQualifiers[0] != "from_address_bar" || v.Referrer.URL != "HTTP://Example.TEST/prev#x" {
		t.Error("the visit payload aliases the Visit")
	}
	d := fullDownload()
	dp := d.Payload()
	dp["url_chain"].([]any)[0] = "changed"
	dp["raw"].(map[string]any)["id"] = "changed"
	if d.URLChain[0] != "HTTP://Example.TEST/redirect" || d.Raw["id"] != int64(9) {
		t.Error("the download payload aliases the Download")
	}
	s := fullSearch()
	sp := s.Payload()
	sp["raw"].(map[string]any)["url_id"] = "changed"
	if s.Raw["url_id"] != int64(4) {
		t.Error("the search payload aliases the Search")
	}
	// the pointer targets are copied, not pointed to
	v.DurationS = ptr(7.0)
	p = v.Payload()
	*v.DurationS = 9
	if p["duration_s"] != 7.0 {
		t.Errorf("duration_s followed a later change of the pointer: %v", p["duration_s"])
	}
	a, b := fullVisit().Payload(), fullVisit().Payload()
	a["raw"].(map[string]any)["transition"] = "changed"
	if b["raw"].(map[string]any)["transition"] == "changed" {
		t.Error("payloads share state")
	}
}

func TestVisitBodyAndSummary(t *testing.T) {
	bodies := []struct {
		v    web.Visit
		want string
	}{
		{web.Visit{URL: verbatimURL, Title: "Example  page"}, verbatimURL + "\nExample  page"},
		{web.Visit{URL: "https://example.test/#frag", Title: ""}, "https://example.test/#frag"},
		{web.Visit{URL: "", Title: "only a title"}, "only a title"},
		{web.Visit{}, ""},
		{web.Visit{URL: " u ", Title: " t "}, " u \n t "}, // exactly as stored: nothing trimmed
	}
	for _, tc := range bodies {
		if got := web.VisitBody(tc.v); got != tc.want {
			t.Errorf("VisitBody(%q, %q) = %q, want %q", tc.v.URL, tc.v.Title, got, tc.want)
		}
	}
	// a URL fragment is findable in the body (the substring index reads it)
	if !strings.Contains(web.VisitBody(web.Visit{URL: "https://example.test/p#section-42"}), "#section-42") {
		t.Error("the fragment is not in the body")
	}

	summaries := []struct {
		v    web.Visit
		want string
	}{
		{web.Visit{URL: "https://example.test/", Title: "Example"}, "Example"},
		{web.Visit{URL: "https://example.test/", Title: ""}, "https://example.test/"},
		{web.Visit{URL: "https://example.test/", Title: " \t\n "}, "https://example.test/"},
		{web.Visit{URL: "u", Title: "a\nb\t c"}, "a b c"},
		{web.Visit{}, ""},
	}
	for _, tc := range summaries {
		if got := web.VisitSummary(tc.v); got != tc.want {
			t.Errorf("VisitSummary(%q, %q) = %q, want %q", tc.v.URL, tc.v.Title, got, tc.want)
		}
	}
	// 80 characters (runes), cut on a character boundary, with the usual marker
	cjk := strings.Repeat(string(rune(0x65E5))+string(rune(0x672C)), 100)
	for _, v := range []web.Visit{{URL: "u", Title: cjk}, {URL: cjk}} {
		got := web.VisitSummary(v)
		if n := utf8.RuneCountInString(got); n > 80 || n < 70 || !utf8.ValidString(got) || !strings.HasSuffix(got, "...") {
			t.Errorf("summary of a long title/url: %d characters, %q", n, got)
		}
	}
	// format and bidi characters are shown, never dropped or passed through
	rlo, zwsp, bom := string(rune(0x202E)), string(rune(0x200B)), string(rune(0xFEFF))
	if got := web.VisitSummary(web.Visit{URL: "u", Title: rlo + "Ba" + zwsp + "nk" + bom}); got != "<U+202E>Ba<U+200B>nk<U+FEFF>" {
		t.Errorf("hidden characters are not visible: %q", got)
	}
	if web.VisitSummary(web.Visit{URL: "u", Title: "Bank"}) == web.VisitSummary(web.Visit{URL: "u", Title: "Ba" + zwsp + "nk"}) {
		t.Error("two different titles give the same summary")
	}
	if got := web.VisitSummary(web.Visit{URL: "https://example.test/" + rlo + "evil" + "\x00"}); strings.ContainsAny(got, rlo+"\x00") {
		t.Errorf("summary of a hostile url = %q", got)
	}
	// always under the records 512-byte cap, even with four-byte characters
	if got := web.VisitSummary(web.Visit{URL: strings.Repeat(string(rune(0x1F600)), 1000)}); len(got) > 512 || !utf8.ValidString(got) {
		t.Errorf("summary of 1000 emoji is %d bytes", len(got))
	}
}

func TestWebDecodeOlderVersions(t *testing.T) {
	t.Run("version constants equal the registry", func(t *testing.T) {
		for _, name := range []string{web.VisitType, web.SearchType, web.DownloadType} {
			ty, ok := records.LookupType(name)
			if !ok || ty.PayloadVersion != web.PayloadVersion {
				t.Errorf("%s: registered %+v (%v), package constant %d", name, ty, ok, web.PayloadVersion)
			}
		}
	})

	t.Run("round trip equals the input", func(t *testing.T) {
		for name, v := range map[string]web.Visit{"minimal": minimalVisit(), "full": fullVisit()} {
			b, err := json.Marshal(v.Payload())
			if err != nil {
				t.Fatal(err)
			}
			got, err := web.DecodeVisit(web.PayloadVersion, b)
			if err != nil || !reflect.DeepEqual(got, v) || !reflect.DeepEqual(got.Payload(), v.Payload()) {
				t.Errorf("visit %s: round trip\n got  %+v (%v)\n want %+v", name, got, err, v)
			}
		}
		for name, s := range map[string]web.Search{"minimal": minimalSearch(), "full": fullSearch()} {
			b, err := json.Marshal(s.Payload())
			if err != nil {
				t.Fatal(err)
			}
			got, err := web.DecodeSearch(web.PayloadVersion, b)
			if err != nil || !reflect.DeepEqual(got, s) || !reflect.DeepEqual(got.Payload(), s.Payload()) {
				t.Errorf("search %s: round trip\n got  %+v (%v)\n want %+v", name, got, err, s)
			}
		}
		for name, d := range map[string]web.Download{"minimal": minimalDownload(), "full": fullDownload()} {
			b, err := json.Marshal(d.Payload())
			if err != nil {
				t.Fatal(err)
			}
			got, err := web.DecodeDownload(web.PayloadVersion, b)
			if err != nil || !reflect.DeepEqual(got, d) || !reflect.DeepEqual(got.Payload(), d.Payload()) {
				t.Errorf("download %s: round trip\n got  %+v (%v)\n want %+v", name, got, err, d)
			}
		}
	})

	t.Run("v1 fixture bytes decode", func(t *testing.T) {
		v, err := web.DecodeVisit(1, []byte(visitFixture))
		want := web.Visit{
			URL: "https://example.test/Path?q=1#Frag", Browser: "chrome", Title: "Example", VisitID: "42", Profile: "Default", Transition: "link",
			TransitionQualifiers: []string{"from_address_bar"}, Referrer: &web.Referrer{VisitID: "41", URL: "https://example.test/"},
			RedirectSource: "40", RedirectDestination: "43", DurationS: ptr(12.5), URLVisitCount: ptr(int64(3)), TypedCount: ptr(int64(1)),
			StatusCode: ptr(int64(200)), Hidden: ptr(false), LoadSuccessful: ptr(true),
			Raw:      map[string]any{"transition": int64(805306368), "ratio": 0.5, "tags": []any{"a"}},
			Recovery: map[string]any{"relation": "absent-from-live", "wal": map[string]any{"frame": int64(3), "committed": true}},
			Snapshot: map[string]any{"name": "snap-1", "xid": int64(12)}, Deleted: map[string]any{"source": "history_tombstones"},
		}
		if err != nil || !reflect.DeepEqual(v, want) {
			t.Errorf("DecodeVisit(fixture) = %+v, %v\n want %+v", v, err, want)
		}
		s, err := web.DecodeSearch(1, []byte(searchFixture))
		wantS := web.Search{Term: "Cats", NormalizedTerm: "cats", Engine: "Example", EngineID: "2", URL: "https://example.test/s?q=Cats", VisitID: "42", TimeSource: "urls.last_visit_time", Browser: "edge", Raw: map[string]any{"url_id": int64(7)}}
		if err != nil || !reflect.DeepEqual(s, wantS) {
			t.Errorf("DecodeSearch(fixture) = %+v, %v\n want %+v", s, err, wantS)
		}
		d, err := web.DecodeDownload(1, []byte(downloadFixture))
		wantD := web.Download{
			TargetPath: "/sdcard/Download/a.pdf", CurrentPath: "/sdcard/Download/a.pdf.crdownload", URL: "https://example.test/a.pdf", Referrer: "https://example.test/",
			TabURL: "https://example.test/tab", Mime: "application/pdf", State: "complete", DangerType: "not_dangerous", InterruptReason: "none", Browser: "chrome",
			URLChain: []string{"https://example.test/r", "https://example.test/a.pdf"}, TotalBytes: ptr(int64(100)), ReceivedBytes: ptr(int64(100)), Opened: ptr(true),
			Raw: map[string]any{"id": int64(3)},
		}
		if err != nil || !reflect.DeepEqual(d, wantD) {
			t.Errorf("DecodeDownload(fixture) = %+v, %v\n want %+v", d, err, wantD)
		}
		for name, tc := range map[string]struct {
			validate func(map[string]any) error
			doc      string
		}{"visit": {web.ValidateVisit, visitFixture}, "search": {web.ValidateSearch, searchFixture}, "download": {web.ValidateDownload, downloadFixture}} {
			var p map[string]any
			dec := json.NewDecoder(strings.NewReader(tc.doc))
			dec.UseNumber()
			if err := dec.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if err := tc.validate(p); err != nil {
				t.Errorf("Validate(%s fixture) = %v", name, err)
			}
		}
	})

	t.Run("a future-added unknown field is ignored", func(t *testing.T) {
		v, err := web.DecodeVisit(1, []byte(`{"url":"u","browser":"safari","new_in_v1_1":{"deep":[1,{"x":null}]},"another":"x"}`))
		if err != nil || v.URL != "u" || v.Browser != "safari" || v.Title != "" || v.Raw != nil || !v.Live() {
			t.Errorf("DecodeVisit = %+v, %v", v, err)
		}
	})

	t.Run("other payload versions are unsupported", func(t *testing.T) {
		for _, v := range []int{0, 2, 3, -1, 1 << 30} {
			if _, err := web.DecodeVisit(v, []byte(visitFixture)); !errors.Is(err, web.ErrUnsupportedPayloadVersion) {
				t.Errorf("DecodeVisit(%d) = %v", v, err)
			}
			if _, err := web.DecodeSearch(v, []byte(searchFixture)); !errors.Is(err, web.ErrUnsupportedPayloadVersion) {
				t.Errorf("DecodeSearch(%d) = %v", v, err)
			}
			if _, err := web.DecodeDownload(v, []byte(downloadFixture)); !errors.Is(err, web.ErrUnsupportedPayloadVersion) {
				t.Errorf("DecodeDownload(%d) = %v", v, err)
			}
		}
		if _, err := web.DecodeVisit(1, []byte(visitFixture)); errors.Is(err, web.ErrUnsupportedPayloadVersion) {
			t.Error("v1 reported as unsupported")
		}
	})

	t.Run("damaged payloads are errors, never a panic and never an unsupported version", func(t *testing.T) {
		const marker = "SECRET-VALUE-9931"
		for name, b := range map[string]string{
			"empty": ``, "not json": `{"url": `, "null": `null`, "array": `[]`, "string": `"x"`,
			"trailing value": `{"url":"u","browser":"chrome"} {}`, "trailing garbage": `{"url":"u","browser":"chrome"} x`,
			"missing browser": `{"url":"u"}`, "missing url": `{"browser":"chrome"}`,
			"wrong type": `{"url":"u","browser":"chrome","hidden":"` + marker + `"}`,
			"enum":       `{"url":"u","browser":"` + marker + `"}`,
			"big number": `{"url":"u","browser":"chrome","status_code":1e999}`,
			"exponent":   `{"url":"u","browser":"chrome","url_visit_count":1e3}`,
			"decimal id": `{"url":"u","browser":"chrome","visit_id":2.0}`,
		} {
			_, err := web.DecodeVisit(1, []byte(b))
			if err == nil {
				t.Errorf("visit %s: no error", name)
				continue
			}
			if errors.Is(err, web.ErrUnsupportedPayloadVersion) || strings.Contains(err.Error(), marker) {
				t.Errorf("visit %s: %v", name, err)
			}
		}
		for _, b := range []string{``, `null`, `[]`, `{"term":""}`, `{"term":"t","engine_id":1.5}`, `{"term":"t","browser":"x"}`} {
			if _, err := web.DecodeSearch(1, []byte(b)); err == nil {
				t.Errorf("search %q: no error", b)
			}
		}
		for _, b := range []string{``, `null`, `[]`, `{}`, `{"mime":"x"}`, `{"url":"u","total_bytes":-1}`, `{"url":"u","total_bytes":1e3}`, `{"url":"u","url_chain":[1]}`} {
			if _, err := web.DecodeDownload(1, []byte(b)); err == nil {
				t.Errorf("download %q: no error", b)
			}
		}
	})

	t.Run("deeply nested input does not panic", func(t *testing.T) {
		deep := strings.Repeat(`{"a":`, 2000) + `1` + strings.Repeat(`}`, 2000)
		if _, err := web.DecodeVisit(1, []byte(`{"url":"u","browser":"chrome","raw":`+deep+`}`)); err == nil {
			t.Error("a payload nested 2000 deep was accepted")
		}
		if _, err := web.DecodeDownload(1, []byte(`{"url":"u","raw":`+deep+`}`)); err == nil {
			t.Error("a download nested 2000 deep was accepted")
		}
	})
}

// TestDecodeKeepsProvenance: a typed reader can never present a recovered,
// snapshot-derived or application-deleted record as a live one.
func TestDecodeKeepsProvenance(t *testing.T) {
	const prov = `"recovery":{"relation":"uncommitted","via":"wal","wal":{"frame":3,"salt1":7,"committed":false},"notes":["a","b"]},` +
		`"snapshot":{"name":"snap-1","xid":12},"deleted":{"source":"history_tombstones"}`
	wantRec := map[string]any{"relation": "uncommitted", "via": "wal", "notes": []any{"a", "b"}, "wal": map[string]any{"frame": int64(3), "salt1": int64(7), "committed": false}}
	wantSnap := map[string]any{"name": "snap-1", "xid": int64(12)}
	wantDel := map[string]any{"source": "history_tombstones"}
	v, err := web.DecodeVisit(1, []byte(`{"url":"u","browser":"chrome",`+prov+`}`))
	s, err2 := web.DecodeSearch(1, []byte(`{"term":"t",`+prov+`}`))
	d, err3 := web.DecodeDownload(1, []byte(`{"url":"u",`+prov+`}`))
	if err != nil || err2 != nil || err3 != nil {
		t.Fatal(err, err2, err3)
	}
	for name, got := range map[string][3]map[string]any{"visit": {v.Recovery, v.Snapshot, v.Deleted}, "search": {s.Recovery, s.Snapshot, s.Deleted}, "download": {d.Recovery, d.Snapshot, d.Deleted}} {
		if !reflect.DeepEqual(got[0], wantRec) || !reflect.DeepEqual(got[1], wantSnap) || !reflect.DeepEqual(got[2], wantDel) {
			t.Errorf("%s: provenance = %#v", name, got)
		}
	}
	if v.Live() || s.Live() || d.Live() {
		t.Error("a record with provenance reads as live")
	}
	if !reflect.DeepEqual(v.Payload()["recovery"], wantRec) || !reflect.DeepEqual(s.Payload()["snapshot"], wantSnap) || !reflect.DeepEqual(d.Payload()["deleted"], wantDel) {
		t.Error("Payload does not write the provenance back")
	}
	// each one alone, and an EMPTY recovery object, still makes it not live
	for _, one := range []string{`"recovery":{}`, `"recovery":{"relation":"superseded-version"}`, `"snapshot":{"name":"s","xid":0}`, `"deleted":{"source":"x"}`} {
		v, err := web.DecodeVisit(1, []byte(`{"url":"u","browser":"chrome",`+one+`}`))
		if err != nil || v.Live() {
			t.Errorf("%s: Live() = %v (%v)", one, v.Live(), err)
		}
		s, err := web.DecodeSearch(1, []byte(`{"term":"t",`+one+`}`))
		if err != nil || s.Live() {
			t.Errorf("%s: search Live() = %v (%v)", one, s.Live(), err)
		}
		d, err := web.DecodeDownload(1, []byte(`{"url":"u",`+one+`}`))
		if err != nil || d.Live() {
			t.Errorf("%s: download Live() = %v (%v)", one, d.Live(), err)
		}
	}
	if !minimalVisit().Live() || !minimalSearch().Live() || !minimalDownload().Live() {
		t.Error("a plain record is not live")
	}
}

const visitFixture = `{
  "browser": "chrome",
  "duration_s": 12.5,
  "future_field": {"anything": [1, 2, 3]},
  "hidden": false,
  "load_successful": true,
  "profile": "Default",
  "raw": {"transition": 805306368, "ratio": 0.5, "tags": ["a"]},
  "recovery": {"relation": "absent-from-live", "wal": {"frame": 3, "committed": true}},
  "redirect": {"destination_visit_id": 43, "source_visit_id": "40"},
  "referrer": {"url": "https://example.test/", "visit_id": 41},
  "snapshot": {"name": "snap-1", "xid": 12},
  "deleted": {"source": "history_tombstones"},
  "status_code": 200,
  "title": "Example",
  "transition": "link",
  "transition_qualifiers": ["from_address_bar"],
  "typed_count": 1,
  "url": "https://example.test/Path?q=1#Frag",
  "url_visit_count": 3,
  "visit_id": 42
}`

const searchFixture = `{
  "browser": "edge",
  "engine": "Example",
  "engine_id": 2,
  "normalized_term": "cats",
  "raw": {"url_id": 7},
  "term": "Cats",
  "time_source": "urls.last_visit_time",
  "url": "https://example.test/s?q=Cats",
  "visit_id": "42",
  "x_future": true
}`

const downloadFixture = `{
  "browser": "chrome",
  "current_path": "/sdcard/Download/a.pdf.crdownload",
  "danger_type": "not_dangerous",
  "future": [1],
  "interrupt_reason": "none",
  "mime": "application/pdf",
  "opened": true,
  "raw": {"id": 3},
  "received_bytes": 100,
  "referrer": "https://example.test/",
  "state": "complete",
  "tab_url": "https://example.test/tab",
  "target_path": "/sdcard/Download/a.pdf",
  "total_bytes": 100,
  "url": "https://example.test/a.pdf",
  "url_chain": ["https://example.test/r", "https://example.test/a.pdf"]
}`

// liveProbe lets the fuzz targets ask any decoded type whether it reads as live.
type liveProbe interface{ Live() bool }

func fuzzSeeds(f *testing.F, seeds ...map[string]any) {
	for _, p := range seeds {
		b, err := json.Marshal(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	for _, s := range []string{
		`{}`, `null`, `[]`, `{"url":"","browser":"chrome"}`, `{"term":""}`, `{"url":"u","browser":"chrome","duration_s":-0.0}`,
		`{"url":"u","browser":"chrome","duration_s":1e999}`, `{"url":"u","browser":"chrome","visit_id":1e3}`, `{"url":"u","browser":"chrome","visit_id":-0}`,
		`{"url":"u","browser":"chrome","visit_id":9223372036854775808}`, `{"url":"u","total_bytes":9223372036854775807,"received_bytes":0}`,
		`{"url":"u","browser":"chrome","recovery":{},"deleted":{"source":"x"},"snapshot":{"name":"s","xid":2}}`,
		`{"term":"t","recovery":{},"deleted":{"source":"x"},"snapshot":{"name":"s","xid":0}}`,
		`{"target_path":"/a","recovery":{"relation":"uncommitted"},"snapshot":{"name":"s","xid":2.0}}`,
		`{"url":"u","browser":"chrome","raw":{"a":{"b":{"c":{}}}}}`,
	} {
		f.Add([]byte(s))
	}
}

// fuzzOne holds what every web fuzz target asserts about one input.
func fuzzOne(t *testing.T, data []byte, validate func(map[string]any) error, decode func() (liveProbe, map[string]any, error)) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return
	}
	if err := validate(m); err != nil {
		if len(err.Error()) > 1<<12 {
			t.Errorf("error too long: %d bytes", len(err.Error()))
		}
		return
	}
	got, payload, err := decode()
	if err != nil {
		// only trailing data after the first value can make a validated object fail to decode
		if len(strings.TrimSpace(string(data[dec.InputOffset():]))) == 0 {
			t.Fatalf("an accepted payload does not decode: %v", err)
		}
		return
	}
	if err := validate(payload); err != nil {
		t.Fatalf("the decoded record does not produce a valid payload: %v", err)
	}
	prov := false
	for _, k := range []string{"recovery", "snapshot", "deleted"} {
		if _, ok := m[k].(map[string]any); ok {
			prov = true
			if _, ok := payload[k].(map[string]any); !ok {
				t.Fatalf("Decode dropped %s", k)
			}
		}
	}
	if got.Live() == prov {
		t.Fatalf("Live() = %v for a payload whose provenance presence is %v", got.Live(), prov)
	}
}

func FuzzWebVisitValidate(f *testing.F) {
	fuzzSeeds(f, minimalVisit().Payload(), fullVisit().Payload())
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzOne(t, data, web.ValidateVisit, func() (liveProbe, map[string]any, error) {
			v, err := web.DecodeVisit(web.PayloadVersion, data)
			if err == nil {
				if again, err2 := web.DecodeVisit(web.PayloadVersion, data); err2 != nil || !reflect.DeepEqual(v, again) {
					t.Fatalf("Decode is not deterministic: %v", err2)
				}
				if v.DurationS != nil && (*v.DurationS < 0 || math.IsNaN(*v.DurationS) || math.IsInf(*v.DurationS, 0)) {
					t.Fatalf("a validated visit decoded to a duration of %v", *v.DurationS)
				}
				if s, b := web.VisitSummary(v), web.VisitBody(v); len(s) > 512 || strings.ContainsAny(s, "\n\r\x00") || !utf8.ValidString(s) || !utf8.ValidString(b) && utf8.ValidString(v.URL+v.Title) {
					t.Fatalf("summary %q or body breaks its contract", s)
				}
			}
			return v, v.Payload(), err
		})
	})
}

func FuzzWebSearchValidate(f *testing.F) {
	fuzzSeeds(f, minimalSearch().Payload(), fullSearch().Payload())
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzOne(t, data, web.ValidateSearch, func() (liveProbe, map[string]any, error) {
			s, err := web.DecodeSearch(web.PayloadVersion, data)
			if err == nil {
				if again, err2 := web.DecodeSearch(web.PayloadVersion, data); err2 != nil || !reflect.DeepEqual(s, again) {
					t.Fatalf("Decode is not deterministic: %v", err2)
				}
			}
			return s, s.Payload(), err
		})
	})
}

func FuzzWebDownloadValidate(f *testing.F) {
	fuzzSeeds(f, minimalDownload().Payload(), fullDownload().Payload())
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzOne(t, data, web.ValidateDownload, func() (liveProbe, map[string]any, error) {
			d, err := web.DecodeDownload(web.PayloadVersion, data)
			if err == nil {
				if again, err2 := web.DecodeDownload(web.PayloadVersion, data); err2 != nil || !reflect.DeepEqual(d, again) {
					t.Fatalf("Decode is not deterministic: %v", err2)
				}
				if d.TotalBytes != nil && *d.TotalBytes < 0 || d.ReceivedBytes != nil && *d.ReceivedBytes < 0 || len(d.TargetPath) > 4096 || len(d.CurrentPath) > 4096 {
					t.Fatalf("a validated download decoded out of bounds: %+v", d)
				}
			}
			return d, d.Payload(), err
		})
	})
}
