package records_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func TestStatsGroupCap(t *testing.T) {
	restore := records.SetMaxStatGroups(3)
	defer restore()
	c := recordstest.NewCase(t)
	for i := 0; i < 5; i++ {
		id := recordstest.AddArtifact(t, c, fmt.Sprintf("a%d.db", i), append([]byte{byte(i + 1)}, mib[1:]...)).ID
		recordstest.Ingest(t, c, records.Parser{Name: fmt.Sprintf("p%d", i), Version: "1"}, []string{id}, recordstest.Records(id, 10, int64(i)))
	}
	r := newReader(t, c)

	rows, err := r.Stats(ctx, records.Filter{}, "artifact")
	if !errors.Is(err, records.ErrStatsGroupCap) || rows != nil {
		t.Fatalf("Stats over 5 groups with a cap of 3: %v, %v", rows, err)
	}
	top, err := r.StatsTop(ctx, records.Filter{}, "artifact", 3)
	if err != nil || len(top.Rows) != 3 || !top.More {
		t.Fatalf("StatsTop(3): %+v, %v", top, err)
	}
	all, err := r.StatsTop(ctx, records.Filter{}, "artifact", 5)
	if err != nil || len(all.Rows) != 5 || all.More {
		t.Fatalf("StatsTop(5): %+v, %v", all, err)
	}
	for i := range top.Rows { // the first groups of the same key order, with the same counts
		if top.Rows[i].Key != all.Rows[i].Key || top.Rows[i].Count != all.Rows[i].Count {
			t.Fatalf("group %d differs: %+v vs %+v", i, top.Rows[i], all.Rows[i])
		}
	}
	// a key with few groups is untouched by the cap
	if byDeleted, err := r.Stats(ctx, records.Filter{}, "deleted"); err != nil || len(byDeleted) == 0 || len(byDeleted) > 3 {
		t.Fatalf("Stats by deleted (at most 3 groups: live, deleted, recovered): %v, %v", byDeleted, err)
	}
	for _, bad := range []int{0, -1, records.MaxStatGroups + 1} {
		if _, err := r.StatsTop(ctx, records.Filter{}, "type", bad); !errors.Is(err, records.ErrInvalidPage) {
			t.Errorf("StatsTop limit %d: %v", bad, err)
		}
	}
}
