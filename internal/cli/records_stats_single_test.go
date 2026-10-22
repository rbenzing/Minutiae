package cli

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

// `records stats --by type` is answered by one covering pass; its output is the output of the previous
// two calls (Overview, then StatsTop), byte for byte, also when --max-groups cuts the groups.
func TestRecordsStatsByTypeEqualsThePreviousTwoCalls(t *testing.T) {
	rc := recDataset(t, true)
	limits := []int{1000, 2, 1}
	want := map[int]string{}
	c, r, err := openRecords(rc.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, max := range limits {
		ov, err := r.Overview(t.Context(), records.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.StatsTop(t.Context(), records.Filter{}, "type", max)
		if err != nil {
			t.Fatal(err)
		}
		ix, err := r.IndexStatus(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		printStats(&b, ov, ix, "type", res.Rows)
		if res.More {
			fmt.Fprintf(&b, "... more groups exist; showing the first %d (raise --max-groups up to %d or narrow the filters)\n", max, records.MaxStatGroups)
		}
		want[max] = b.String()
	}
	if err := c.Close(); err != nil { // the CLI takes the case lock
		t.Fatal(err)
	}
	for _, max := range limits {
		code, got := run(t, Deps{}, "records", "stats", "--case", rc.dir, "--max-groups", fmt.Sprint(max))
		if code != ExitOK || got != want[max] {
			t.Fatalf("--max-groups %d: code %d\n--- got\n%s--- want (Overview + StatsTop)\n%s", max, code, got, want[max])
		}
	}
}
