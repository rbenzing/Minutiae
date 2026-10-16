package records_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func allRows(t *testing.T, r *records.Reader) []records.Row {
	t.Helper()
	var out []records.Row
	cur := ""
	for {
		res, err := r.List(ctx, records.Filter{}, records.Page{Limit: records.MaxLimit, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, res.Rows...)
		if res.NextCursor == "" {
			return out
		}
		cur = res.NextCursor
	}
}

func twoArtifacts(t *testing.T) (*evidence.Case, *records.Reader, [2]string) {
	t.Helper()
	c := recordstest.NewCase(t)
	var ids [2]string
	for i := range ids {
		ids[i] = recordstest.AddArtifact(t, c, "a"+string(rune('0'+i))+".db", append([]byte{byte(i + 1)}, mib[1:]...)).ID
		recordstest.Ingest(t, c, records.Parser{Name: "p" + string(rune('0'+i)), Version: "1"}, []string{ids[i]}, recordstest.Records(ids[i], 200, int64(i+1)))
	}
	return c, newReader(t, c), ids
}

func TestMultiValueListEqualsOracle(t *testing.T) {
	_, r, art := twoArtifacts(t)
	all := wantOrder(allRows(t, r))
	in := func(set []string, v string) bool { return slices.Contains(set, v) }
	types := []string{"message", "call", "message", "web_visit"} // a duplicated value is one value
	cases := []struct {
		name string
		f    records.Filter
		keep func(records.Row) bool
	}{
		{"types", records.Filter{Types: types}, func(x records.Row) bool { return in(types, x.Type) }},
		{"artifacts", records.Filter{ArtifactIDs: art[:]}, func(x records.Row) bool { return in(art[:], x.ArtifactID) }},
		{"types and one artifact", records.Filter{Types: types, ArtifactIDs: art[:1]}, func(x records.Row) bool { return in(types, x.Type) && x.ArtifactID == art[0] }},
	}
	for _, tc := range cases {
		var want []records.Row
		for _, x := range all {
			if tc.keep(x) {
				want = append(want, x)
			}
		}
		for _, desc := range []bool{false, true} {
			w := slices.Clone(want)
			if desc {
				slices.Reverse(w)
			}
			for _, limit := range []int{1, 7, 100} {
				var got []int64
				cur := ""
				for pages := 0; ; pages++ {
					res, err := r.List(ctx, tc.f, records.Page{Limit: limit, Cursor: cur, Desc: desc})
					if err != nil {
						t.Fatalf("%s desc=%v limit=%d: %v", tc.name, desc, limit, err)
					}
					got = append(got, ids(res.Rows)...)
					if res.NextCursor == "" {
						break
					}
					cur = res.NextCursor
					if pages > 10_000 {
						t.Fatal("paging does not end")
					}
				}
				if !slices.Equal(got, ids(w)) {
					t.Fatalf("%s desc=%v limit=%d: pages differ from the oracle (%d vs %d rows)", tc.name, desc, limit, len(got), len(w))
				}
			}
		}
	}
}

// A merged page reads at most one limit+1 window per value: the deterministic bound on its work.
func TestMergedPageReadsAreBoundedPerValue(t *testing.T) {
	_, r, _ := twoArtifacts(t)
	var logged []string
	r.SetQueryLog(func(q string) { logged = append(logged, q) })
	before := r.RowsRead()
	if _, err := r.List(ctx, records.Filter{Types: []string{"message", "call", "web_visit"}}, records.Page{Limit: 10}); err != nil {
		t.Fatal(err)
	}
	got := r.RowsRead() - before
	if got == 0 || got > 3*(10+1) {
		t.Fatalf("a 3-value page read %d rows, want 1..%d", got, 3*11)
	}
	single := 0
	for _, q := range logged {
		if strings.Contains(q, "r.type IN (") {
			t.Fatalf("a 3-value list still runs one IN query:\n%s", q)
		}
		if strings.Contains(q, "r.type = ?") {
			single++
		}
	}
	if single < 3 {
		t.Fatalf("expected one single-type query per value, got %d", single)
	}
}

func TestLongListFallsBackToOneInQuery(t *testing.T) {
	_, r, _ := twoArtifacts(t)
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, "t"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	many = append(many, "message")
	n := 0
	r.SetQueryLog(func(q string) {
		if strings.Contains(q, "r.type IN (") {
			n++
		}
	})
	res, err := r.List(ctx, records.Filter{Types: many}, records.Page{Limit: 5})
	if err != nil || len(res.Rows) == 0 || n != 1 {
		t.Fatalf("41 values: %d rows, %d IN queries, %v", len(res.Rows), n, err)
	}
}

// A time window (untimed rows excluded, or included) pages exactly as the oracle filter does.
func TestTimeWindowListEqualsOracle(t *testing.T) {
	_, r, _ := twoArtifacts(t)
	all := wantOrder(allRows(t, r))
	var lo, hi int64
	n := 0
	for _, x := range all {
		if us, ok := micros(x); ok {
			if n == 0 {
				lo = us
			}
			hi = us
			n++
		}
	}
	if n < 20 || hi <= lo {
		t.Fatalf("dataset has %d timed rows spanning %d..%d", n, lo, hi)
	}
	from, to := time.UnixMicro(lo+(hi-lo)/4).UTC(), time.UnixMicro(lo+(hi-lo)*3/4).UTC()
	for _, untimed := range []bool{false, true} {
		f := records.Filter{From: &from, To: &to, IncludeUntimed: untimed}
		var want []records.Row
		for _, x := range all {
			us, ok := micros(x)
			if (ok && us >= from.UnixMicro() && us < to.UnixMicro()) || (!ok && untimed) {
				want = append(want, x)
			}
		}
		if len(want) == 0 {
			t.Fatal("empty oracle")
		}
		for _, desc := range []bool{false, true} {
			w := slices.Clone(want)
			if desc {
				slices.Reverse(w)
			}
			for _, limit := range []int{1, 9, 100} {
				var got []int64
				cur := ""
				for pages := 0; pages < 10_000; pages++ {
					res, err := r.List(ctx, f, records.Page{Limit: limit, Cursor: cur, Desc: desc})
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, ids(res.Rows)...)
					if res.NextCursor == "" {
						break
					}
					cur = res.NextCursor
				}
				if !slices.Equal(got, ids(w)) {
					t.Fatalf("untimed=%v desc=%v limit=%d: %d rows, oracle %d", untimed, desc, limit, len(got), len(w))
				}
			}
		}
	}
}
