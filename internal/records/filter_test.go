package records_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

type fx struct {
	label   string
	art     int // index into the artifacts
	parser  int // index into fxParsers
	typ     string
	hour    int // -1 = untimed
	deleted bool
	recover string
	conf    int // -1 = none
	path    string
}

var fxParsers = []records.Parser{
	{Name: "alpha", Version: "1", Hash: "h1"},
	{Name: "alpha", Version: "2", Hash: "h2"},
	{Name: "beta", Version: "1"},
}

// Each (parser name, artifact) pair is covered by one run, so nothing is superseded.
var fxRows = []fx{
	{label: "r01", art: 0, parser: 0, typ: "message", hour: 1, conf: 50, path: "/data/50%_off/x.db"},
	{label: "r02", art: 0, parser: 0, typ: "message", hour: 3, conf: 90, path: "/data/50%/y"},
	{label: "r03", art: 0, parser: 0, typ: "call", hour: 6, conf: -1, path: "/data/50X/z"},
	{label: "r04", art: 0, parser: 0, typ: "call", hour: -1, conf: 60, path: "/data/a_b/1"},
	{label: "r05", art: 0, parser: 0, typ: "contact", hour: 4, deleted: true, conf: 70, path: "/data/aXb/2"},
	{label: "r06", art: 0, parser: 0, typ: "contact", hour: 5, deleted: true, recover: "carve", conf: 80, path: `/data/back\slash/3`},
	{label: "r07", art: 1, parser: 1, typ: "message", hour: 3, conf: 40, path: "/data/été/4"},
	{label: "r08", art: 1, parser: 1, typ: "location", hour: 6, deleted: true, recover: "carve", conf: 100, path: "/data/etE/5"},
	{label: "r09", art: 1, parser: 1, typ: "location", hour: -1, conf: 0},
	{label: "r10", art: 0, parser: 2, typ: "web_visit", hour: 2, conf: 61, path: "/DATA/upper"},
	{label: "r11", art: 0, parser: 2, typ: "web_visit", hour: -1, deleted: true, conf: -1},
	{label: "r12", art: 0, parser: 2, typ: "message", hour: 3, conf: 99, path: "/data/50%_off/x.db"},
}

type fixture struct {
	c      *evidence.Case
	r      *records.Reader
	arts   []evidence.ManifestRecord
	ingest map[int]string // parser index -> ingest id
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	c := recordstest.NewCase(t)
	fix := fixture{c: c, ingest: map[int]string{}, arts: []evidence.ManifestRecord{
		recordstest.AddArtifact(t, c, "a1.db", mib),
		recordstest.AddArtifact(t, c, "a2.db", mib),
	}}
	for pi, p := range fxParsers {
		var recs []records.Record
		covered := map[string]bool{}
		for _, row := range fxRows {
			if row.parser != pi {
				continue
			}
			rec := records.Record{
				Type: row.typ, ArtifactID: fix.arts[row.art].ID, Summary: row.label, SourcePath: row.path,
				Deleted: row.deleted, Recovery: row.recover, Payload: map[string]any{},
			}
			if row.hour >= 0 {
				rec.Time = &records.Time{T: hourTime(row.hour)}
			}
			if row.conf >= 0 {
				rec.Confidence = ptr(row.conf)
			}
			recs = append(recs, rec)
			covered[fix.arts[row.art].ID] = true
		}
		var cover []string
		for id := range covered {
			cover = append(cover, id)
		}
		fix.ingest[pi] = recordstest.Ingest(t, c, p, cover, recs).IngestID
	}
	fix.r = newReader(t, c)
	return fix
}

// expect returns the labels of the fixture rows for which keep is true.
func expect(keep func(fx) bool) []string {
	out := []string{}
	for _, r := range fxRows {
		if keep(r) {
			out = append(out, r.label)
		}
	}
	slices.Sort(out)
	return out
}

func sameSet(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

func TestFilterMatrix(t *testing.T) {
	f := newFixture(t)
	h := func(n int) *time.Time { return ptr(hourTime(n)) }
	cases := []struct {
		name string
		f    records.Filter
		keep func(fx) bool
	}{
		{"all", records.Filter{}, func(fx) bool { return true }},
		{"one type", records.Filter{Types: []string{"message"}}, func(r fx) bool { return r.typ == "message" }},
		{"two types", records.Filter{Types: []string{"call", "location"}}, func(r fx) bool { return r.typ == "call" || r.typ == "location" }},
		{"unknown type", records.Filter{Types: []string{"nope"}}, func(fx) bool { return false }},
		{"from", records.Filter{From: h(3)}, func(r fx) bool { return r.hour >= 3 }},
		{"from keeps untimed when asked", records.Filter{From: h(3), IncludeUntimed: true}, func(r fx) bool { return r.hour >= 3 || r.hour < 0 }},
		{"to is exclusive", records.Filter{To: h(3)}, func(r fx) bool { return r.hour >= 0 && r.hour < 3 }},
		{"half open range", records.Filter{From: h(3), To: h(6)}, func(r fx) bool { return r.hour >= 3 && r.hour < 6 }},
		{"half open range with untimed", records.Filter{From: h(3), To: h(6), IncludeUntimed: true}, func(r fx) bool { return r.hour < 0 || r.hour >= 3 && r.hour < 6 }},
		{"empty range", records.Filter{From: h(3), To: h(3)}, func(fx) bool { return false }},
		{"untimed without a bound matches everything", records.Filter{IncludeUntimed: true}, func(fx) bool { return true }},
		{"artifact 1", records.Filter{ArtifactIDs: []string{f.arts[0].ID}}, func(r fx) bool { return r.art == 0 }},
		{"artifact 2", records.Filter{ArtifactIDs: []string{f.arts[1].ID}}, func(r fx) bool { return r.art == 1 }},
		{"both artifacts", records.Filter{ArtifactIDs: []string{f.arts[1].ID, f.arts[0].ID}}, func(fx) bool { return true }},
		{"path with percent", records.Filter{PathPrefix: "/data/50%"}, func(r fx) bool { return strings.HasPrefix(r.path, "/data/50%") }},
		{"path with underscore", records.Filter{PathPrefix: "/data/a_b"}, func(r fx) bool { return strings.HasPrefix(r.path, "/data/a_b") }},
		{"path with backslash", records.Filter{PathPrefix: `/data/back\`}, func(r fx) bool { return strings.HasPrefix(r.path, `/data/back\`) }},
		{"path non-ASCII", records.Filter{PathPrefix: "/data/é"}, func(r fx) bool { return strings.HasPrefix(r.path, "/data/é") }},
		{"path is case sensitive", records.Filter{PathPrefix: "/data/E"}, func(fx) bool { return false }},
		{"path upper", records.Filter{PathPrefix: "/DATA"}, func(r fx) bool { return strings.HasPrefix(r.path, "/DATA") }},
		{"path whole value", records.Filter{PathPrefix: "/data/50%/y"}, func(r fx) bool { return r.path == "/data/50%/y" }},
		{"path longer than any", records.Filter{PathPrefix: "/data/50%/y/and/more"}, func(fx) bool { return false }},
		{"path root", records.Filter{PathPrefix: "/"}, func(r fx) bool { return r.path != "" }},
		{"deleted only", records.Filter{Deleted: records.Only}, func(r fx) bool { return r.deleted }},
		{"deleted none", records.Filter{Deleted: records.None}, func(r fx) bool { return !r.deleted }},
		{"recovered only", records.Filter{Recovered: records.Only}, func(r fx) bool { return r.recover != "" }},
		{"recovered none", records.Filter{Recovered: records.None}, func(r fx) bool { return r.recover == "" }},
		{"deleted but not recovered", records.Filter{Deleted: records.Only, Recovered: records.None}, func(r fx) bool { return r.deleted && r.recover == "" }},
		{"parser by name", records.Filter{Parsers: []records.ParserRef{{Name: "alpha"}}}, func(r fx) bool { return r.parser <= 1 }},
		{"parser by name and version", records.Filter{Parsers: []records.ParserRef{{Name: "alpha", Version: "2"}}}, func(r fx) bool { return r.parser == 1 }},
		{"parser wrong version", records.Filter{Parsers: []records.ParserRef{{Name: "beta", Version: "2"}}}, func(fx) bool { return false }},
		{"two parsers", records.Filter{Parsers: []records.ParserRef{{Name: "alpha", Version: "1"}, {Name: "beta"}}}, func(r fx) bool { return r.parser != 1 }},
		{"min confidence excludes NULL", records.Filter{MinConfidence: ptr(60)}, func(r fx) bool { return r.conf >= 60 }},
		{"min confidence zero still excludes NULL", records.Filter{MinConfidence: ptr(0)}, func(r fx) bool { return r.conf >= 0 }},
		{"min confidence 100", records.Filter{MinConfidence: ptr(100)}, func(r fx) bool { return r.conf == 100 }},
		{"ingest", records.Filter{IngestID: f.ingest[1]}, func(r fx) bool { return r.parser == 1 }},
		{"ingest and type", records.Filter{IngestID: f.ingest[0], Types: []string{"call"}}, func(r fx) bool { return r.parser == 0 && r.typ == "call" }},
		{"unknown ingest", records.Filter{IngestID: "no-such-ingest"}, func(fx) bool { return false }},
		{"everything at once", records.Filter{
			Types: []string{"message", "contact"}, From: h(1), To: h(6), IncludeUntimed: true, ArtifactIDs: []string{f.arts[0].ID},
			Deleted: records.None, Recovered: records.None, Parsers: []records.ParserRef{{Name: "alpha"}, {Name: "beta"}}, MinConfidence: ptr(50),
		}, func(r fx) bool {
			return (r.typ == "message" || r.typ == "contact") && (r.hour < 0 || r.hour >= 1 && r.hour < 6) && r.art == 0 &&
				!r.deleted && r.recover == "" && r.conf >= 50
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := expect(tc.keep)
			res, err := f.r.List(ctx, tc.f, records.Page{Limit: records.MaxLimit})
			if err != nil {
				t.Fatal(err)
			}
			if got := summaries(res.Rows); !sameSet(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
			n, capped, err := f.r.Count(ctx, tc.f, 0)
			if err != nil || capped || int(n) != len(want) {
				t.Fatalf("Count = %d, %v, %v; want %d", n, capped, err, len(want))
			}
			// the same selection in descending pages of 2
			var desc []records.Row
			for cur := ""; ; {
				p, err := f.r.List(ctx, tc.f, records.Page{Limit: 2, Cursor: cur, Desc: true})
				if err != nil {
					t.Fatal(err)
				}
				desc = append(desc, p.Rows...)
				if cur = p.NextCursor; cur == "" {
					break
				}
			}
			if got := summaries(desc); !sameSet(got, want) {
				t.Fatalf("descending pages: got %v, want %v", got, want)
			}
		})
	}
}

func TestListRowFields(t *testing.T) {
	f := newFixture(t)
	res, err := f.r.List(ctx, records.Filter{Types: []string{"location"}, Deleted: records.Only}, records.Page{})
	if err != nil || len(res.Rows) != 1 {
		t.Fatal(err, res.Rows)
	}
	row := res.Rows[0]
	if row.Summary != "r08" || row.Type != "location" || !row.Deleted || !row.Recovered || row.Method != "carve" ||
		row.Confidence == nil || *row.Confidence != 100 || row.SourcePath != "/data/etE/5" ||
		row.Parser != (records.ParserInfo{Name: "alpha", Version: "2", Hash: "h2"}) || row.IngestID != f.ingest[1] ||
		row.ArtifactID != f.arts[1].ID || row.Superseded || row.Locator != "" || row.Range != nil || row.PayloadV != 1 {
		t.Fatalf("row = %+v", row)
	}
	if row.TS == nil || !row.TS.T.Equal(hourTime(6)) || row.TS.Basis != records.BasisUTC || row.TSEnd != nil {
		t.Fatalf("times = %+v end %+v", row.TS, row.TSEnd)
	}
	res, err = f.r.List(ctx, records.Filter{Parsers: []records.ParserRef{{Name: "beta"}}, Types: []string{"web_visit"}, Deleted: records.None}, records.Page{})
	if err != nil || len(res.Rows) != 1 || res.Rows[0].Parser.Hash != "" || res.Rows[0].Confidence == nil {
		t.Fatal(err, res.Rows)
	}
	res, err = f.r.List(ctx, records.Filter{Deleted: records.Only, Types: []string{"web_visit"}}, records.Page{})
	if err != nil || len(res.Rows) != 1 || res.Rows[0].Confidence != nil || res.Rows[0].TS != nil || res.Rows[0].SourcePath != "" {
		t.Fatal(err, res.Rows)
	}
}

func TestFilterInjectionIsInert(t *testing.T) {
	f := newFixture(t)
	evil := []string{"'; DROP TABLE records; --", `"`, "'", "a' OR '1'='1", "%", "_", `\`, "x\x00y", "é' --", "?", "?1", ")) OR 1=1 --"}
	for _, e := range evil {
		for name, fl := range map[string]records.Filter{
			"types":    {Types: []string{e}},
			"artifact": {ArtifactIDs: []string{e}},
			"path":     {PathPrefix: strings.ReplaceAll(e, "\x00", "")}, // a NUL prefix is ErrInvalidFilter (TestPathPrefixInvalidTextIsErrInvalidFilter)
			"parser":   {Parsers: []records.ParserRef{{Name: e}}},
			"version":  {Parsers: []records.ParserRef{{Name: "alpha", Version: e}}},
			"ingest":   {IngestID: e},
		} {
			res, err := f.r.List(ctx, fl, records.Page{})
			if err != nil {
				t.Fatalf("%s %q: %v", name, e, err)
			}
			// "%", "_" and quotes are literal text: no path or name starts with them
			if len(res.Rows) != 0 {
				t.Fatalf("%s %q matched %d rows", name, e, len(res.Rows))
			}
		}
	}
	if n, _, err := f.r.Count(ctx, records.Filter{}, 0); err != nil || int(n) != len(fxRows) {
		t.Fatalf("after the attacks Count = %d, %v", n, err)
	}
	if n := count(t, f.c, "records"); n != len(fxRows) {
		t.Fatalf("records table holds %d rows", n)
	}

	big := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("v%d", i)
		}
		return out
	}
	bigParsers := func(n int) []records.ParserRef {
		out := make([]records.ParserRef, n)
		for i := range out {
			out[i] = records.ParserRef{Name: fmt.Sprintf("p%d", i)}
		}
		return out
	}
	for name, fl := range map[string]records.Filter{
		"types":     {Types: big(1001)},
		"artifacts": {ArtifactIDs: big(1001)},
		"parsers":   {Parsers: bigParsers(1001)},
	} {
		if _, err := f.r.List(ctx, fl, records.Page{}); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("%s: 1001 values: %v, want ErrInvalidFilter", name, err)
		}
		if _, _, err := f.r.Count(ctx, fl, 0); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("%s: Count with 1001 values: %v, want ErrInvalidFilter", name, err)
		}
		if _, err := f.r.Stats(ctx, fl, "type"); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("%s: Stats with 1001 values: %v, want ErrInvalidFilter", name, err)
		}
		if _, err := f.r.Overview(ctx, fl); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("%s: Overview with 1001 values: %v, want ErrInvalidFilter", name, err)
		}
	}
	for name, fl := range map[string]records.Filter{
		"types":     {Types: big(1000)},
		"artifacts": {ArtifactIDs: big(1000)},
		"parsers":   {Parsers: bigParsers(1000)},
	} {
		if _, err := f.r.List(ctx, fl, records.Page{}); err != nil {
			t.Errorf("%s: 1000 values: %v", name, err)
		}
	}
	for name, fl := range map[string]records.Filter{
		"confidence high":  {MinConfidence: ptr(101)},
		"confidence low":   {MinConfidence: ptr(-1)},
		"tri":              {Deleted: records.Tri(7)},
		"recovered tri":    {Recovered: records.Tri(-1)},
		"empty parser":     {Parsers: []records.ParserRef{{Version: "1"}}},
		"invalid path":     {PathPrefix: "\xff\xfe"},
		"time overflow":    {From: ptr(time.Unix(1<<60, 0))},
		"to time overflow": {To: ptr(time.Unix(-(1 << 60), 0))},
	} {
		if _, err := f.r.List(ctx, fl, records.Page{}); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("%s: %v, want ErrInvalidFilter", name, err)
		}
	}
}
