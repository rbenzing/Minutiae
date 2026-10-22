package records_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
)

// One covering pass answers the overview's total, untimed and time range and the by-type groups; every
// field equals what the separate statements answer, on data with untimed, deleted, recovered and
// superseded records.
func TestStatsSinglePassEqualsSeparateStatements(t *testing.T) {
	_, r, _, run1 := shapeDataset(t)
	from, to := time.Unix(1_600_000_000, 0).UTC(), time.Unix(1_600_000_000+20*3600, 0).UTC()
	conf := 50
	filters := map[string]records.Filter{
		"none":              {},
		"one type":          {Types: []string{"message"}},
		"two types":         {Types: []string{"message", "call"}},
		"deleted only":      {Deleted: records.Only},
		"deleted none":      {Deleted: records.None},
		"recovered only":    {Recovered: records.Only},
		"superseded":        {IncludeSuperseded: true},
		"window":            {From: &from, To: &to},
		"window + untimed":  {From: &from, To: &to, IncludeUntimed: true},
		"confidence":        {MinConfidence: &conf},
		"ingest id (run 1)": {IngestID: run1},
		"nothing":           {PathPrefix: "/no/such/prefix"},
		"text":              {Text: compileWords(t, "common")},
		"text + superseded": {Text: compileWords(t, "word3"), IncludeSuperseded: true},
	}
	// the data holds what the comparison needs
	all, err := r.Overview(ctx, records.Filter{IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	def, err := r.Overview(ctx, records.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Untimed == 0 || all.Deleted == 0 || all.Recovered == 0 || def.SupersededRecords == 0 || all.Records <= def.Records {
		t.Fatalf("the data lacks a case: %+v / %+v", all, def)
	}
	for name, f := range filters {
		for _, limit := range []int{1, 2, 1000} {
			wantOv, err := r.Overview(ctx, f)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			wantTop, err := r.StatsTop(ctx, f, "type", limit)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			gotOv, gotTop, err := r.OverviewAndTypes(ctx, f, limit)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !reflect.DeepEqual(gotOv, wantOv) {
				t.Errorf("%s, limit %d: overview %+v, separate statements %+v", name, limit, gotOv, wantOv)
			}
			if !reflect.DeepEqual(gotTop, wantTop) {
				t.Errorf("%s, limit %d: by type %+v, separate statement %+v", name, limit, gotTop, wantTop)
			}
		}
	}
}

func TestStatsSinglePassIsOneCoveringScan(t *testing.T) {
	c, _ := bigDataset(t, 300)
	q, err := records.OverviewTypesQuery(records.Filter{}, false)
	if err != nil {
		t.Fatal(err)
	}
	plan := explain(t, c, q.SQL, q.Args)
	if !strings.Contains(plan, "COVERING INDEX records_type_ts") || strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("not one covering scan of records_type_ts:\n%s\n%s", q.SQL, plan)
	}
}

func TestStatsSinglePassRefusesRank(t *testing.T) {
	_, r, _, _ := shapeDataset(t)
	q, err := records.CompileQuery("common", records.TextOptions{Rank: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.OverviewAndTypes(ctx, records.Filter{Text: q}, 10); err == nil {
		t.Fatal("a relevance-ordered query was accepted")
	}
	if _, _, err := r.OverviewAndTypes(ctx, records.Filter{}, 0); err == nil {
		t.Fatal("a group limit of 0 was accepted")
	}
}
