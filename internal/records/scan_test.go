package records_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// heart is U+2764, a symbol the word index cannot hold.
var heart = cp(0x2764)

// scanTexts span the folds the trigram tokenizer applies: case, composed and decomposed accents,
// sharp s, fullwidth, CJK, Hangul, emoji with and without the variation selector, Turkish dotted I,
// no-break spaces, and an empty text.
var scanTexts = []string{
	"Hello World", "hello world again", "HELLO there general", "caf" + cp(0xe9) + " au lait", "cafe" + cp(0x301) + " noir",
	"Stra" + cp(0xdf) + "e nach Wien", "STRASSE and more", cp(0xff21, 0xff22, 0xff23, 0xff11, 0xff12, 0xff13) + " wide",
	cp(0x65e5, 0x672c, 0x8a9e, 0x306e, 0x30c6, 0x30ad, 0x30b9, 0x30c8), cp(0x6771, 0x4eac, 0x30bf, 0x30ef, 0x30fc),
	cp(0xc548, 0xb155, 0xd558, 0xc138, 0xc694) + " " + cp(0xc138, 0xacc4), "I " + heart + " you", "I " + heart + cp(0xfe0f) + " you", cp(0x1f600) + " smile",
	cp(0x130) + "stanbul trip", "a" + cp(0xa0) + "b" + cp(0xa0) + "c test", "c++ and xaby", "price " + cp(0x20ac) + "5", "",
}

// scanCase ingests 40 records over scanTexts (summary and body from different texts, tied and missing
// times, two types).
func scanCase(t *testing.T) (*evidence.Case, *records.Reader) {
	t.Helper()
	c, art := setup(t)
	var recs []records.Record
	for i := 0; i < 40; i++ {
		typ := "note"
		if i%3 == 0 {
			typ = "event"
		}
		r := records.Record{
			Type: typ, ArtifactID: art.ID, Payload: recordstest.ValidPayload(typ),
			Summary: scanTexts[i%len(scanTexts)], Body: scanTexts[(i*7+3)%len(scanTexts)],
		}
		if i%5 != 0 {
			r.Time = &records.Time{T: time.Unix(1_600_000_000+int64(i/4)*3600, 0).UTC()}
		}
		recs = append(recs, r)
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	return c, newReader(t, c)
}

// pagedIDs gathers the ids of every page of a search, in the order the pages returned them.
func pagedIDs(t *testing.T, r *records.Reader, f records.Filter, desc bool, limit int) []int64 {
	t.Helper()
	var all []int64
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("too many pages")
		}
		res, err := r.Search(ctx, f, records.Page{Limit: limit, Cursor: cursor, Desc: desc}, records.SearchOptions{})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		all = append(all, hitIDs(res.Hits)...)
		if res.NextCursor == "" {
			return all
		}
		cursor = res.NextCursor
	}
}

func scanOpts(o records.TextOptions) records.TextOptions {
	o.Substring, o.Scan = true, true
	return o
}

func runeWindows(s string, n int) []string {
	rs := []rune(s)
	var out []string
	for i := 0; i+n <= len(rs); i++ {
		out = append(out, string(rs[i:i+n]))
	}
	return out
}

func TestScanAgreesWithTheTrigramIndex(t *testing.T) {
	_, r := scanCase(t)
	var needles []string
	for _, s := range scanTexts[:10] {
		needles = append(needles, runeWindows(s, 3)...)
		needles = append(needles, runeWindows(s, 4)...)
	}
	needles = append(needles, "zzz", "qqqq", "no such text", heart+heart+heart, "xyzzy", "abcdefg", "plugh", "quux", "frobnicate", "Wien Wien")
	tested := 0
	for _, needle := range needles {
		for _, col := range []records.Column{records.ColAny, records.ColSummary, records.ColBody} {
			idx, err := records.CompileQuery(needle, records.TextOptions{Substring: true, Column: col})
			if err != nil {
				continue // the index refuses it; there is nothing to agree with
			}
			scan, err := records.CompileQuery(needle, scanOpts(records.TextOptions{Column: col}))
			if err != nil {
				t.Fatalf("scan refuses %q that the index accepts: %v", needle, err)
			}
			for _, f := range []records.Filter{{}, {Types: []string{"note"}}} {
				for _, desc := range []bool{false, true} {
					fi, fs := f, f
					fi.Text, fs.Text = idx, scan
					want := pagedIDs(t, r, fi, desc, 7)
					if got := pagedIDs(t, r, fs, desc, 7); !slices.Equal(got, want) {
						t.Fatalf("needle %q column %d desc %v types %v: scan %v, index %v", needle, col, desc, f.Types, got, want)
					}
				}
			}
			tested++
		}
	}
	if tested < 100 {
		t.Fatalf("only %d needle/column pairs were compared", tested)
	}
}

func TestScanFindsShortAndSymbolNeedles(t *testing.T) {
	c, art := setup(t)
	texts := []string{"I " + heart + " you", "I " + heart + cp(0xfe0f) + " you", "plain", "xaby", cp(0x20ac) + "5", "c++", "other"}
	var recs []records.Record
	for _, s := range texts {
		recs = append(recs, records.Record{Type: "note", ArtifactID: art.ID, Summary: s, Payload: recordstest.ValidPayload("note")})
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	r := newReader(t, c)
	find := func(needle string) []int64 {
		t.Helper()
		q, err := records.CompileQuery(needle, scanOpts(records.TextOptions{}))
		if err != nil {
			t.Fatalf("CompileQuery(%q): %v", needle, err)
		}
		return pagedIDs(t, r, records.Filter{Text: q}, false, 50)
	}
	for needle, want := range map[string][]int64{
		heart: {1, 2}, heart + cp(0xfe0f): {1, 2}, "ab": {4}, cp(0x20ac): {5}, "c+": {6},
	} {
		if got := find(needle); !slices.Equal(got, want) {
			t.Errorf("scan for %q: %v, want %v", needle, got, want)
		}
	}
	for _, bad := range []string{"  ", cp(0x301)} {
		if _, err := records.CompileQuery(bad, scanOpts(records.TextOptions{})); !errors.Is(err, records.ErrInvalidQuery) {
			t.Errorf("CompileQuery(%q) = %v, want ErrInvalidQuery", bad, err)
		}
	}
}

func TestScanPagesAreKeysetPagesInListOrder(t *testing.T) {
	c, art := setup(t)
	var recs []records.Record
	for i := 0; i < 40; i++ {
		s := "other"
		if i%2 == 0 || i%5 == 0 {
			s = "needle here"
		}
		r := records.Record{Type: "note", ArtifactID: art.ID, Summary: s, Payload: recordstest.ValidPayload("note")}
		if i%4 != 0 {
			r.Time = &records.Time{T: time.Unix(1_600_000_000+int64(i/3)*60, 0).UTC()}
		}
		recs = append(recs, r)
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	r := newReader(t, c)
	q := mustCompile(t, "needle", scanOpts(records.TextOptions{}))
	for _, desc := range []bool{false, true} {
		var got []int64
		cursor := ""
		for {
			res, err := r.Search(ctx, records.Filter{Text: q}, records.Page{Limit: 4, Cursor: cursor, Desc: desc}, records.SearchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, hitIDs(res.Hits)...)
			if res.NextCursor == "" {
				break
			}
			cursor = res.NextCursor
		}
		all, err := r.List(ctx, records.Filter{}, records.Page{Limit: records.MaxLimit, Desc: desc})
		if err != nil {
			t.Fatal(err)
		}
		var want []int64
		for _, row := range all.Rows {
			if row.Summary == "needle here" {
				want = append(want, row.ID)
			}
		}
		if len(want) < 20 || !slices.Equal(got, want) {
			t.Errorf("desc %v: pages %v, list order %v", desc, got, want)
		}
		seen := map[int64]bool{}
		for _, id := range got {
			if seen[id] {
				t.Errorf("id %d appears twice", id)
			}
			seen[id] = true
		}
	}
	res, err := r.Search(ctx, records.Filter{Text: q}, records.Page{Limit: 4}, records.SearchOptions{})
	if err != nil || res.NextCursor == "" {
		t.Fatalf("first page: %v %q", err, res.NextCursor)
	}
	sub := mustCompile(t, "needle", records.TextOptions{Substring: true})
	if _, err := r.Search(ctx, records.Filter{Text: sub}, records.Page{Limit: 4, Cursor: res.NextCursor}, records.SearchOptions{}); !errors.Is(err, records.ErrBadCursor) {
		t.Errorf("scan cursor on a substring search: %v, want ErrBadCursor", err)
	}
	res2, err := r.Search(ctx, records.Filter{Text: sub}, records.Page{Limit: 4}, records.SearchOptions{})
	if err != nil || res2.NextCursor == "" {
		t.Fatalf("substring first page: %v", err)
	}
	if _, err := r.Search(ctx, records.Filter{Text: q}, records.Page{Limit: 4, Cursor: res2.NextCursor}, records.SearchOptions{}); !errors.Is(err, records.ErrBadCursor) {
		t.Errorf("substring cursor on a scan: %v, want ErrBadCursor", err)
	}
}

func TestScanDoesNotNeedACurrentIndex(t *testing.T) {
	c, r := scanCase(t)
	recordstest.SetFTSNormVersion(t, c.Dir, "x")
	sub := mustCompile(t, "Hello", records.TextOptions{Substring: true})
	if _, err := r.Search(ctx, records.Filter{Text: sub}, records.Page{Limit: 5}, records.SearchOptions{}); !errors.Is(err, records.ErrIndexNotCurrent) {
		t.Fatalf("substring search on a stale index: %v, want ErrIndexNotCurrent", err)
	}
	scan := mustCompile(t, "Hello", scanOpts(records.TextOptions{}))
	res, err := r.Search(ctx, records.Filter{Text: scan}, records.Page{Limit: 5}, records.SearchOptions{})
	if err != nil || len(res.Hits) == 0 {
		t.Fatalf("scan on a stale index: %v, %d hits", err, len(res.Hits))
	}
}

func TestScanTimeoutIsErrSearchTimeout(t *testing.T) {
	_, r := scanCase(t)
	f := records.Filter{Text: mustCompile(t, "Hello", scanOpts(records.TextOptions{}))}
	_, err := r.Search(lateCtx{context.Background()}, f, records.Page{Limit: 5}, records.SearchOptions{})
	if !errors.Is(err, records.ErrSearchTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expired deadline: %v, want ErrSearchTimeout wrapping DeadlineExceeded", err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.Search(cctx, f, records.Page{Limit: 5}, records.SearchOptions{})
	if !errors.Is(err, context.Canceled) || errors.Is(err, records.ErrSearchTimeout) {
		t.Errorf("cancelled context: %v, want context.Canceled and not ErrSearchTimeout", err)
	}
	// a deadline that passes after the first statement started, in the middle of the scan
	e := newExpiringCtx()
	r.SetAfterStart(e.expire)
	_, err = r.Search(e, f, records.Page{Limit: 5}, records.SearchOptions{})
	if !errors.Is(err, records.ErrSearchTimeout) {
		t.Errorf("deadline passing during the scan: %v, want ErrSearchTimeout", err)
	}
}

func TestScanRefusesABodyAboveTheIngestCap(t *testing.T) {
	c, art := setup(t)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, []records.Record{
		{Type: "note", ArtifactID: art.ID, Summary: "one", Body: "body", Payload: recordstest.ValidPayload("note")},
	})
	r := newReader(t, c)
	recordstest.SetRecordColumn(t, c.Dir, 1, "body", strings.Repeat("a", 4<<20+1))
	f := records.Filter{Text: mustCompile(t, "zzz", scanOpts(records.TextOptions{}))}
	_, err := r.Search(ctx, f, records.Page{Limit: 5}, records.SearchOptions{})
	if !errors.Is(err, evidence.ErrIntegrity) || !strings.Contains(err.Error(), "record 1") {
		t.Fatalf("over-cap body: %v, want ErrIntegrity naming record 1", err)
	}
}

func TestScanQueryIsOnlyForSearch(t *testing.T) {
	_, r := scanCase(t)
	f := records.Filter{Text: mustCompile(t, "Hello", scanOpts(records.TextOptions{}))}
	if _, err := r.List(ctx, f, records.Page{Limit: 5}); !errors.Is(err, records.ErrInvalidFilter) || !strings.Contains(err.Error(), "a scan query is not for List or Count") {
		t.Errorf("List: %v", err)
	}
	if _, _, err := r.Count(ctx, f, 0); !errors.Is(err, records.ErrInvalidFilter) || !strings.Contains(err.Error(), "a scan query is not for List or Count") {
		t.Errorf("Count: %v", err)
	}
}

func TestScanOptionsAreChecked(t *testing.T) {
	if _, err := records.CompileQuery("hello", records.TextOptions{Scan: true}); !errors.Is(err, records.ErrInvalidQuery) {
		t.Errorf("Scan without Substring: %v", err)
	}
	if _, err := records.CompileQuery("hello", records.TextOptions{Scan: true, Substring: true, Rank: true}); !errors.Is(err, records.ErrInvalidQuery) {
		t.Errorf("Scan with Rank: %v", err)
	}
}

func TestNotIndexableRefusalsWrapErrNotIndexable(t *testing.T) {
	_, err := records.CompileQuery("ab", records.TextOptions{Substring: true})
	if !errors.Is(err, records.ErrNotIndexable) || !errors.Is(err, records.ErrInvalidQuery) {
		t.Errorf("short substring: %v", err)
	}
	if _, err := records.CompileQuery(heart, records.TextOptions{}); !errors.Is(err, records.ErrNotIndexable) {
		t.Errorf("symbol-only word: %v", err)
	}
	for name, in := range map[string]string{"short prefix": "a*", "long term": strings.Repeat("a", 300)} {
		_, err := records.CompileQuery(in, records.TextOptions{})
		if !errors.Is(err, records.ErrInvalidQuery) || errors.Is(err, records.ErrNotIndexable) {
			t.Errorf("%s: %v, want ErrInvalidQuery and not ErrNotIndexable", name, err)
		}
	}
}

func TestScanCountsRecordsRead(t *testing.T) {
	c, art := setup(t)
	var recs []records.Record
	for i := 0; i < 15; i++ {
		typ := "note"
		if i >= 10 {
			typ = "event"
		}
		recs = append(recs, records.Record{Type: typ, ArtifactID: art.ID, Summary: "plain text", Body: "more", Payload: recordstest.ValidPayload(typ)})
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	r := newReader(t, c)
	f := records.Filter{Types: []string{"note"}, Text: mustCompile(t, "zzz", scanOpts(records.TextOptions{}))}
	res, err := r.Search(ctx, f, records.Page{Limit: 5}, records.SearchOptions{})
	if err != nil || len(res.Hits) != 0 || res.Scanned != 10 || res.NextCursor != "" {
		t.Fatalf("hits %d scanned %d cursor %q err %v, want 0 hits, 10 scanned, no cursor", len(res.Hits), res.Scanned, res.NextCursor, err)
	}
}

func TestScanSnippetHighlightsShortNeedle(t *testing.T) {
	c, art := setup(t)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, []records.Record{
		{Type: "note", ArtifactID: art.ID, Summary: "I " + heart + " you", Payload: recordstest.ValidPayload("note")},
	})
	r := newReader(t, c)
	f := records.Filter{Text: mustCompile(t, heart, scanOpts(records.TextOptions{}))}
	res, err := r.Search(ctx, f, records.Page{Limit: 5}, records.SearchOptions{Snippets: true})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].Summary == nil {
		t.Fatalf("search: %v %+v", err, res.Hits)
	}
	found := false
	for _, sp := range res.Hits[0].Summary.Spans {
		if sp.Match && sp.Text == heart {
			found = true
		}
	}
	if !found {
		t.Errorf("spans %+v hold no matched heart", res.Hits[0].Summary.Spans)
	}
}

// TestScanChecksTheBodySizeWhateverMatched (LX6-T6-2): an over-cap body is an integrity error even on
// a record whose summary matches, and in a summary-only search.
func TestScanChecksTheBodySizeWhateverMatched(t *testing.T) {
	c, art := setup(t)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, []records.Record{
		{Type: "note", ArtifactID: art.ID, Summary: "needle", Body: "body", Payload: recordstest.ValidPayload("note")},
	})
	r := newReader(t, c)
	recordstest.SetRecordColumn(t, c.Dir, 1, "body", strings.Repeat("a", 4<<20+1))
	for _, col := range []records.Column{records.ColAny, records.ColSummary} {
		f := records.Filter{Text: mustCompile(t, "needle", scanOpts(records.TextOptions{Column: col}))}
		_, err := r.Search(ctx, f, records.Page{Limit: 5}, records.SearchOptions{})
		if !errors.Is(err, evidence.ErrIntegrity) || !strings.Contains(err.Error(), "record 1") {
			t.Errorf("column %d: %v, want ErrIntegrity naming record 1", col, err)
		}
	}
}
