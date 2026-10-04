package records_test

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func newReader(t *testing.T, c *evidence.Case) *records.Reader {
	t.Helper()
	r, err := records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// bigDataset returns a case holding n records with tied and NULL timestamps.
func bigDataset(t *testing.T, n int) (*evidence.Case, *records.Reader) {
	t.Helper()
	c, art := setup(t)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recordstest.Records(art.ID, n, 7))
	return c, newReader(t, c)
}

func ids(rows []records.Row) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func micros(r records.Row) (int64, bool) {
	if r.TS == nil {
		return 0, false
	}
	return r.TS.T.UnixMicro(), true
}

// wantOrder sorts rows by (untimed last, ts, id): the order the reader promises.
func wantOrder(rows []records.Row) []records.Row {
	out := slices.Clone(rows)
	slices.SortFunc(out, func(a, b records.Row) int {
		ta, aok := micros(a)
		tb, bok := micros(b)
		switch {
		case aok != bok:
			if aok {
				return -1
			}
			return 1
		case ta != tb:
			if ta < tb {
				return -1
			}
			return 1
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out
}

// pages walks every page of a listing and returns the rows in the order met.
func pages(t *testing.T, r *records.Reader, f records.Filter, size int, desc bool, maxRows int) []records.Row {
	t.Helper()
	var all []records.Row
	cur := ""
	for i := 0; ; i++ {
		if i > maxRows+2 {
			t.Fatalf("size %d desc=%v: more pages than rows (a cursor does not advance)", size, desc)
		}
		res, err := r.List(ctx, f, records.Page{Limit: size, Cursor: cur, Desc: desc})
		if err != nil {
			t.Fatalf("size %d desc=%v page %d: %v", size, desc, i, err)
		}
		if len(res.Rows) == 0 {
			t.Fatalf("size %d desc=%v page %d: an empty page", size, desc, i)
		}
		if len(res.Rows) > size {
			t.Fatalf("size %d desc=%v page %d: %d rows", size, desc, i, len(res.Rows))
		}
		all = append(all, res.Rows...)
		if res.NextCursor == "" {
			return all
		}
		if len(res.Rows) != size {
			t.Fatalf("size %d desc=%v page %d: a short page (%d) with a next cursor", size, desc, i, len(res.Rows))
		}
		cur = res.NextCursor
	}
}

func TestKeysetPaginationStableWithTiesAndNulls(t *testing.T) {
	_, r := bigDataset(t, 150)
	base := int64(1_600_000_000)
	filters := map[string]records.Filter{
		"none":       {},
		"types":      {Types: []string{"message", "call", "event"}},
		"deleted":    {Deleted: records.Only},
		"live":       {Deleted: records.None, From: ptr(time.Unix(base+10*3600, 0)), IncludeUntimed: true},
		"timedwin":   {From: ptr(time.Unix(base+5*3600, 0)), To: ptr(time.Unix(base+30*3600, 0))},
		"singletype": {Types: []string{"message"}},
	}
	sizes := []int{1, 2, 3, 4, 5, 6, 7, 100}
	for name, f := range filters {
		t.Run(name, func(t *testing.T) {
			whole, err := r.List(ctx, f, records.Page{Limit: records.MaxLimit})
			if err != nil {
				t.Fatal(err)
			}
			if whole.NextCursor != "" || len(whole.Rows) == 0 {
				t.Fatalf("whole listing: %d rows, cursor %q", len(whole.Rows), whole.NextCursor)
			}
			// the order is the promised one, checked independently of SQL
			if want := wantOrder(whole.Rows); !reflect.DeepEqual(ids(want), ids(whole.Rows)) {
				t.Fatalf("order is not (untimed last, ts, id):\n got %v\nwant %v", ids(whole.Rows), ids(want))
			}
			revWhole, err := r.List(ctx, f, records.Page{Limit: records.MaxLimit, Desc: true})
			if err != nil {
				t.Fatal(err)
			}
			rev := slices.Clone(whole.Rows)
			slices.Reverse(rev)
			if !reflect.DeepEqual(ids(rev), ids(revWhole.Rows)) {
				t.Fatalf("descending is not the exact reverse:\n got %v\nwant %v", ids(revWhole.Rows), ids(rev))
			}
			if name == "none" {
				seenNull, seenTie := false, false
				prev, havePrev := int64(0), false
				for _, row := range whole.Rows {
					m, timed := micros(row)
					seenNull = seenNull || !timed
					seenTie = seenTie || timed && havePrev && m == prev
					prev, havePrev = m, timed
				}
				if !seenNull || !seenTie {
					t.Fatalf("dataset lacks NULL (%v) or tied (%v) timestamps", seenNull, seenTie)
				}
			}
			for _, size := range sizes {
				asc := pages(t, r, f, size, false, len(whole.Rows))
				if !reflect.DeepEqual(asc, whole.Rows) {
					t.Fatalf("size %d ascending: pages differ from the un-paged query\n got %v\nwant %v", size, ids(asc), ids(whole.Rows))
				}
				desc := pages(t, r, f, size, true, len(whole.Rows))
				if !reflect.DeepEqual(desc, revWhole.Rows) {
					t.Fatalf("size %d descending: pages differ from the un-paged query\n got %v\nwant %v", size, ids(desc), ids(revWhole.Rows))
				}
				if again := pages(t, r, f, size, false, len(whole.Rows)); !reflect.DeepEqual(again, asc) {
					t.Fatalf("size %d: an unchanged database gave different pages", size)
				}
			}
		})
	}
}

func TestListDefaultOrderIsTimeThenID(t *testing.T) {
	c, art := setup(t)
	at := func(h int) *records.Time { return &records.Time{T: time.Unix(1_700_000_000+int64(h)*3600, 0).UTC()} }
	specs := []struct {
		label string
		ts    *records.Time
	}{{"u1", nil}, {"t5a", at(5)}, {"t2", at(2)}, {"u2", nil}, {"t5b", at(5)}, {"t9", at(9)}, {"u3", nil}, {"t5c", at(5)}}
	recs := make([]records.Record, len(specs)) // insertion order is the id order
	for i, s := range specs {
		recs[i] = records.Record{Type: "message", ArtifactID: art.ID, Summary: s.label, Time: s.ts, Payload: map[string]any{}}
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	r := newReader(t, c)
	label := func(rows []records.Row) string {
		var l []string
		for _, row := range rows {
			l = append(l, row.Summary)
		}
		return strings.Join(l, " ")
	}
	asc, err := r.List(ctx, records.Filter{}, records.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := label(asc.Rows), "t2 t5a t5b t5c t9 u1 u2 u3"; got != want {
		t.Fatalf("ascending: %s, want %s", got, want)
	}
	desc, err := r.List(ctx, records.Filter{}, records.Page{Desc: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := label(desc.Rows), "u3 u2 u1 t9 t5c t5b t5a t2"; got != want {
		t.Fatalf("descending: %s, want %s", got, want)
	}
}

func TestListLimits(t *testing.T) {
	_, r := bigDataset(t, 150)
	res, err := r.List(ctx, records.Filter{}, records.Page{})
	if err != nil || len(res.Rows) != 100 || res.NextCursor == "" {
		t.Fatalf("default page: %d rows, cursor %q, %v", len(res.Rows), res.NextCursor, err)
	}
	for _, n := range []int{-1, records.MaxLimit + 1, 1 << 40} {
		if _, err := r.List(ctx, records.Filter{}, records.Page{Limit: n}); !errors.Is(err, records.ErrInvalidPage) {
			t.Errorf("limit %d: %v, want ErrInvalidPage", n, err)
		}
	}
	res, err = r.List(ctx, records.Filter{}, records.Page{Limit: records.MaxLimit})
	if err != nil || len(res.Rows) != 150 || res.NextCursor != "" {
		t.Fatalf("max page: %d rows, cursor %q, %v", len(res.Rows), res.NextCursor, err)
	}
	// exactly a full page and nothing after it carries no cursor
	res, err = r.List(ctx, records.Filter{}, records.Page{Limit: 150})
	if err != nil || len(res.Rows) != 150 || res.NextCursor != "" {
		t.Fatalf("exact page: %d rows, cursor %q, %v", len(res.Rows), res.NextCursor, err)
	}
}

func TestBadCursorIsErrBadCursor(t *testing.T) {
	_, r := bigDataset(t, 30)
	asc, err := r.List(ctx, records.Filter{}, records.Page{Limit: 5})
	if err != nil || asc.NextCursor == "" {
		t.Fatal(err, asc.NextCursor)
	}
	desc, err := r.List(ctx, records.Filter{}, records.Page{Limit: 5, Desc: true})
	if err != nil || desc.NextCursor == "" {
		t.Fatal(err, desc.NextCursor)
	}
	enc := func(js string) string { return "v1." + b64(strings.ReplaceAll(js, "@", testFP)) }
	cases := []struct {
		name string
		cur  string
		desc bool
	}{
		{"junk", "garbage", false},
		{"only the prefix", "v1.", false},
		{"wrong version", "v2." + strings.TrimPrefix(asc.NextCursor, "v1."), false},
		{"no version", strings.TrimPrefix(asc.NextCursor, "v1."), false},
		{"not base64", "v1.!!!!", false},
		{"padded base64", asc.NextCursor + "=", false},
		{"standard alphabet", "v1.+/+/", false},
		{"base64 of non-JSON", "v1." + b64("not json at all"), false},
		{"base64 of JSON null", enc("null"), false},
		{"base64 of an array", enc("[1,2,3,4]"), false},
		{"missing id", enc(`{"d":0,"n":0,"ts":5,"f":"@"}`), false},
		{"missing ts", enc(`{"d":0,"n":0,"id":5,"f":"@"}`), false},
		{"missing d", enc(`{"n":0,"ts":5,"id":5,"f":"@"}`), false},
		{"missing n", enc(`{"d":0,"ts":5,"id":5,"f":"@"}`), false},
		{"extra field", enc(`{"d":0,"n":0,"ts":5,"id":5,"x":1,"f":"@"}`), false},
		{"duplicate field", enc(`{"d":0,"n":0,"ts":5,"id":5,"id":6,"f":"@"}`), false},
		{"reordered", enc(`{"n":0,"d":0,"ts":5,"id":5,"f":"@"}`), false},
		{"whitespace", enc(`{"d":0, "n":0,"ts":5,"id":5,"f":"@"}`), false},
		{"upper-case key", enc(`{"D":0,"n":0,"ts":5,"id":5,"f":"@"}`), false},
		{"trailing data", enc(`{"d":0,"n":0,"ts":5,"id":5,"f":"@"}x`), false},
		{"missing fingerprint", enc(`{"d":0,"n":0,"ts":5,"id":5}`), false},
		{"empty fingerprint", enc(`{"d":0,"n":0,"ts":5,"id":5,"f":""}`), false},
		{"short fingerprint", enc(`{"d":0,"n":0,"ts":5,"id":5,"f":"0123"}`), false},
		{"long fingerprint", enc(`{"d":0,"n":0,"ts":5,"id":5,"f":"@@"}`), false},
		{"upper-case fingerprint", enc(`{"d":0,"n":0,"ts":5,"id":5,"f":"0123456789ABCDEF0123456789ABCDEF"}`), false},
		{"non-hex fingerprint", enc(`{"d":0,"n":0,"ts":5,"id":5,"f":"0123456789abcdefghijklmnopqrstuv"}`), false},
		{"numeric fingerprint", enc(`{"d":0,"n":0,"ts":5,"id":5,"f":5}`), false},
		{"another listing's fingerprint", enc(`{"d":0,"n":0,"ts":5,"id":5,"f":"@"}`), false},
		{"absurd ts", enc(`{"d":0,"n":0,"ts":1e30,"id":5,"f":"@"}`), false},
		{"ts beyond int64", enc(`{"d":0,"n":0,"ts":9223372036854775808,"id":5,"f":"@"}`), false},
		{"fractional ts", enc(`{"d":0,"n":0,"ts":1.5,"id":5,"f":"@"}`), false},
		{"string ts", enc(`{"d":0,"n":0,"ts":"5","id":5,"f":"@"}`), false},
		{"absurd id", enc(`{"d":0,"n":0,"ts":5,"id":99999999999999999999,"f":"@"}`), false},
		{"zero id", enc(`{"d":0,"n":0,"ts":5,"id":0,"f":"@"}`), false},
		{"negative id", enc(`{"d":0,"n":0,"ts":5,"id":-3,"f":"@"}`), false},
		{"direction 2", enc(`{"d":2,"n":0,"ts":5,"id":5,"f":"@"}`), false},
		{"null flag 2", enc(`{"d":0,"n":2,"ts":5,"id":5,"f":"@"}`), false},
		{"untimed with a ts", enc(`{"d":0,"n":1,"ts":5,"id":5,"f":"@"}`), false},
		{"descending cursor on an ascending page", desc.NextCursor, false},
		{"ascending cursor on a descending page", asc.NextCursor, true},
		{"too long", "v1." + strings.Repeat("A", 5000), false},
		{"unicode", "v1.é", false},
		{"NUL", "v1.\x00", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := r.List(ctx, records.Filter{}, records.Page{Limit: 5, Cursor: tc.cur, Desc: tc.desc})
			if !errors.Is(err, records.ErrBadCursor) {
				t.Fatalf("got %v (%d rows), want ErrBadCursor", err, len(res.Rows))
			}
			if len(res.Rows) != 0 {
				t.Fatalf("a bad cursor returned %d rows", len(res.Rows))
			}
		})
	}
	// the genuine cursors still work in their own direction
	if _, err := r.List(ctx, records.Filter{}, records.Page{Limit: 5, Cursor: asc.NextCursor}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.List(ctx, records.Filter{}, records.Page{Limit: 5, Cursor: desc.NextCursor, Desc: true}); err != nil {
		t.Fatal(err)
	}
}

func TestCountCapped(t *testing.T) {
	_, r := bigDataset(t, 150)
	for _, tc := range []struct {
		limit  int
		n      int64
		capped bool
	}{{40, 40, true}, {149, 149, true}, {150, 150, false}, {151, 150, false}, {1000, 150, false}, {0, 150, false}, {1, 1, true}} {
		n, capped, err := r.Count(ctx, records.Filter{}, tc.limit)
		if err != nil || n != tc.n || capped != tc.capped {
			t.Errorf("Count(limit %d) = %d, %v, %v; want %d, %v", tc.limit, n, capped, err, tc.n, tc.capped)
		}
	}
	if _, _, err := r.Count(ctx, records.Filter{}, -1); !errors.Is(err, records.ErrInvalidPage) {
		t.Errorf("negative limit: %v", err)
	}
	n, capped, err := r.Count(ctx, records.Filter{Types: []string{"message"}}, 5)
	if err != nil || n != 5 || !capped {
		t.Errorf("filtered Count = %d, %v, %v", n, capped, err)
	}
}

// explain returns the query plan of sqlText on a separate read-only connection.
func explain(t *testing.T, c *evidence.Case, sqlText string, args []any) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(c.Dir, "artifacts.db"))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+sqlText, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\n%s", err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "\n")
}

func TestListUsesIndexes(t *testing.T) {
	c, _ := bigDataset(t, 300) // no supersession: record_superseded is empty
	if n := count(t, c, "record_superseded"); n != 0 {
		t.Fatalf("record_superseded holds %d rows", n)
	}
	cur := &records.Cursor{TS: 1_600_000_000_000_000, ID: 5}
	nullCur := &records.Cursor{N: 1, ID: 5}
	for _, tc := range []struct {
		name  string
		f     records.Filter
		cur   *records.Cursor
		desc  bool
		index string
	}{
		{"unfiltered ascending", records.Filter{}, nil, false, "records_ts"},
		{"unfiltered descending", records.Filter{}, nil, true, "records_ts"},
		{"single type ascending", records.Filter{Types: []string{"message"}}, nil, false, "records_type_ts"},
		{"single type descending", records.Filter{Types: []string{"message"}}, nil, true, "records_type_ts"},
		{"unfiltered after a timed row", records.Filter{}, cur, false, "records_ts"},
		{"unfiltered after an untimed row", records.Filter{}, nullCur, false, "records_ts"},
		{"single type after a timed row, descending", records.Filter{Types: []string{"message"}}, &records.Cursor{D: 1, TS: cur.TS, ID: 5}, true, "records_type_ts"},
		{"unfiltered descending after an untimed row", records.Filter{}, &records.Cursor{D: 1, N: 1, ID: 5}, true, "records_ts"},
		{"single type descending after an untimed row", records.Filter{Types: []string{"message"}}, &records.Cursor{D: 1, N: 1, ID: 5}, true, "records_type_ts"},
		{"deleted only", records.Filter{Deleted: records.Only}, nil, false, "records_deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qs, err := records.ListQueries(tc.f, tc.desc, tc.cur, 101, false)
			if err != nil {
				t.Fatal(err)
			}
			for i, q := range qs {
				plan := explain(t, c, q.SQL, q.Args)
				t.Logf("query %d plan:\n%s", i+1, plan)
				if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
					t.Fatalf("query %d: the default order is not served by an index:\n%s", i+1, plan)
				}
				if !strings.Contains(plan, "INDEX "+tc.index) {
					t.Fatalf("query %d: plan does not use %s:\n%s", i+1, tc.index, plan)
				}
				// a page after a cursor seeks to its position instead of scanning from the start
				if tc.cur != nil && strings.Contains(plan, "SCAN r") {
					t.Fatalf("query %d scans the whole index to reach the cursor:\n%s", i+1, plan)
				}
			}
		})
	}
}

func TestNewReaderRefusesV1Case(t *testing.T) {
	c, err := evidence.Open(recordstest.NewV1Case(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	r, err := records.NewReader(c)
	if !errors.Is(err, evidence.ErrNeedsUpgrade) || r != nil {
		t.Fatalf("NewReader on a v1 case: %v, %v", r, err)
	}
}

// two runs of one parser over the same artifact

type twoRuns struct {
	r        *records.Reader
	run1     records.IngestResult
	run2     records.IngestResult
	run1Labs []string
	run2Labs []string
}

func labelled(art evidence.ManifestRecord, prefix string, n int) []records.Record {
	out := make([]records.Record, n)
	for i := range out {
		out[i] = records.Record{
			Type: "message", ArtifactID: art.ID, Summary: fmt.Sprintf("%s%d", prefix, i), Payload: map[string]any{"i": i},
			Time: &records.Time{T: hourTime(i)},
		}
	}
	return out
}

func labs(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	slices.Sort(out)
	return out
}

func hourTime(h int) time.Time { return time.Unix(1_700_000_000+int64(h)*3600, 0).UTC() }

func newTwoRuns(t *testing.T, secondComplete bool) twoRuns {
	t.Helper()
	c, art := setup(t)
	tr := twoRuns{run1Labs: labs("old", 4), run2Labs: labs("new", 3)}
	tr.run1 = recordstest.Ingest(t, c, records.Parser{Name: "sms", Version: "1"}, []string{art.ID}, labelled(art, "old", 4))
	if secondComplete {
		tr.run2 = recordstest.Ingest(t, c, records.Parser{Name: "sms", Version: "2"}, []string{art.ID}, labelled(art, "new", 3))
	} else {
		w := startWriter(t, c, records.Parser{Name: "sms", Version: "2"}, records.WriterOptions{}, art.ID)
		add(t, w, labelled(art, "new", 3))
		if err := w.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		res, err := w.Abort(ctx, errors.New("parser crashed"))
		if err != nil {
			t.Fatal(err)
		}
		tr.run2 = res
	}
	tr.r = newReader(t, c)
	return tr
}

func summaries(rows []records.Row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Summary)
	}
	slices.Sort(out)
	return out
}

func TestSupersedeHidesOlderRunByDefault(t *testing.T) {
	tr := newTwoRuns(t, true)
	res, err := tr.r.List(ctx, records.Filter{}, records.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if got := summaries(res.Rows); !reflect.DeepEqual(got, tr.run2Labs) {
		t.Fatalf("default listing %v, want only the newer run %v", got, tr.run2Labs)
	}
	for _, row := range res.Rows {
		if row.Superseded || row.IngestID != tr.run2.IngestID {
			t.Fatalf("row %+v", row)
		}
	}
	if n, _, err := tr.r.Count(ctx, records.Filter{}, 0); err != nil || n != 3 {
		t.Fatalf("Count = %d, %v", n, err)
	}
	// stats and the overview apply the same rule
	st, err := tr.r.Stats(ctx, records.Filter{}, "run")
	if err != nil || len(st) != 1 || st[0].Key != tr.run2.IngestID {
		t.Fatalf("stats by run: %v, %v", st, err)
	}
	ov, err := tr.r.Overview(ctx, records.Filter{})
	if err != nil || ov.Records != 3 || ov.SupersededRecords != 4 {
		t.Fatalf("overview %+v, %v", ov, err)
	}
	// paging hides them too
	if got := summaries(pages(t, tr.r, records.Filter{}, 2, false, 10)); !reflect.DeepEqual(got, tr.run2Labs) {
		t.Fatalf("paged %v", got)
	}
}

func TestIncludeSupersededShowsAll(t *testing.T) {
	tr := newTwoRuns(t, true)
	f := records.Filter{IncludeSuperseded: true}
	res, err := tr.r.List(ctx, f, records.Page{})
	if err != nil {
		t.Fatal(err)
	}
	all := append(slices.Clone(tr.run1Labs), tr.run2Labs...)
	slices.Sort(all)
	if got := summaries(res.Rows); !reflect.DeepEqual(got, all) {
		t.Fatalf("got %v, want %v", got, all)
	}
	for _, row := range res.Rows {
		if want := row.IngestID == tr.run1.IngestID; row.Superseded != want {
			t.Fatalf("row %s superseded=%v, want %v", row.Summary, row.Superseded, want)
		}
	}
	if n, _, err := tr.r.Count(ctx, f, 0); err != nil || n != 7 {
		t.Fatalf("Count = %d, %v", n, err)
	}
	// asking for one ingest shows it, superseded or not
	res, err = tr.r.List(ctx, records.Filter{IngestID: tr.run1.IngestID}, records.Page{})
	if err != nil || !reflect.DeepEqual(summaries(res.Rows), tr.run1Labs) {
		t.Fatalf("ingest filter: %v, %v", summaries(res.Rows), err)
	}
	for _, row := range res.Rows {
		if !row.Superseded {
			t.Fatalf("row %s of the older run is not flagged superseded", row.Summary)
		}
	}
}

func TestIncompleteRunRecordsStayVisible(t *testing.T) {
	tr := newTwoRuns(t, false)
	if tr.run2.Outcome != "incomplete" {
		t.Fatalf("second run outcome %q", tr.run2.Outcome)
	}
	res, err := tr.r.List(ctx, records.Filter{}, records.Page{})
	if err != nil {
		t.Fatal(err)
	}
	all := append(slices.Clone(tr.run1Labs), tr.run2Labs...)
	slices.Sort(all)
	if got := summaries(res.Rows); !reflect.DeepEqual(got, all) {
		t.Fatalf("an incomplete run hid or lost records: %v, want %v", got, all)
	}
	for _, row := range res.Rows {
		if row.Superseded {
			t.Fatalf("row %s flagged superseded by an incomplete run", row.Summary)
		}
	}
}
