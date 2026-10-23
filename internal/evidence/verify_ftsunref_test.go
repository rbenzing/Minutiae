package evidence

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"testing"
)

// unrefStructure builds a one-level structure record naming the segments (each with the pages 1 to 1).
func unrefStructure(segs []uint64) []byte {
	b := []byte{0, 0, 0, 0}
	b = appendSQLiteVarint(b, 1)                 // levels
	b = appendSQLiteVarint(b, uint64(len(segs))) // segments
	b = appendSQLiteVarint(b, 0)                 // write counter
	b = appendSQLiteVarint(b, 0)                 // merge
	b = appendSQLiteVarint(b, uint64(len(segs))) // segments in the level
	for _, s := range segs {
		b = appendSQLiteVarint(b, s)
		b = appendSQLiteVarint(b, 1)
		b = appendSQLiteVarint(b, 1)
	}
	return b
}

// Which _data rows checkUnreferenced names is decided by the segment ranges of the structure record: the test
// puts rows on both sides of every edge of the gaps (s<<37-1, s<<37, (s+1)<<37-1, (s+1)<<37), at negative ids,
// at the extremes and at the last segment a row id can name (2^26-1), and compares with a brute-force oracle
// that applies the definition to each row alone (D176, after the gap probing of 5D-15a).
func TestCheckUnreferencedBoundaries(t *testing.T) {
	const last = int64(1)<<26 - 1 // the highest segment a 63-bit row id can name
	seg := func(s int64) int64 { return s << ftsSegidShift }
	cases := []struct {
		name string
		segs []uint64
		rows []int64
	}{
		{"no segment", nil, []int64{math.MinInt64, -1 << 40, -1, 0, 2, 11, seg(1) - 1, seg(1), seg(1) + 5, math.MaxInt64}},
		{"negative ids beside a segment", []uint64{3}, []int64{math.MinInt64, -seg(1), -seg(3), -1, 0, seg(3) - 1, seg(3), seg(4) - 1, seg(4)}},
		{"one segment, both edges", []uint64{5}, []int64{seg(5) - 1, seg(5), seg(5) + 1<<31, seg(6) - 1, seg(6), seg(6) + 1, math.MaxInt64}},
		{"adjacent segments leave no gap", []uint64{4, 5, 6}, []int64{seg(4) - 1, seg(4), seg(5) - 1, seg(5), seg(6) - 1, seg(6), seg(7) - 1, seg(7)}},
		{"gaps across several segments", []uint64{2, 3, 7, 100, 4000}, []int64{
			seg(1), seg(2) - 1, seg(2), seg(4) - 1, seg(4), seg(5), seg(7) - 1, seg(7), seg(8) - 1, seg(8),
			seg(99) + 3, seg(100) - 1, seg(100), seg(101) - 1, seg(101), seg(3999), seg(4000), seg(4001) - 1, seg(4001), math.MaxInt64,
		}},
		{"segment zero owns no row", []uint64{0, 9}, []int64{0, 1, 10, 11, seg(1) - 1, seg(1), seg(9), seg(10) - 1, seg(10)}},
		{"the last segment a row id can name", []uint64{1, uint64(last)}, []int64{
			seg(1), seg(2) - 1, seg(2), seg(last) - 1, seg(last), seg(last) + 7, math.MaxInt64,
		}},
		{"the segment before the last one", []uint64{uint64(last - 1)}, []int64{seg(last-1) - 1, seg(last - 1), seg(last) - 1, seg(last), math.MaxInt64}},
		{"a listed segment no row can have", []uint64{uint64(last), uint64(last) + 1, 1 << 30}, []int64{seg(last) - 1, seg(last), math.MaxInt64}},
	}
	row := regexp.MustCompile(`data row (-?\d+)`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := indexedTextCase(t, 20)
			for _, table := range FTSTables() {
				if _, err := c.store.db.Exec(fmt.Sprintf(`DELETE FROM %s_data`, table)); err != nil { //nolint:gosec // a constant table name in a test
					t.Fatal(err)
				}
				if _, err := c.store.db.Exec(fmt.Sprintf(`DELETE FROM %s_idx`, table)); err != nil { //nolint:gosec // a constant table name in a test
					t.Fatal(err)
				}
			}
			table := FTSWordTable
			ins := func(id int64) {
				t.Helper()
				if _, err := c.store.db.Exec(`INSERT OR REPLACE INTO `+table+`_data(id, block) VALUES (?, x'00')`, id); err != nil { //nolint:gosec // a constant table name in a test
					t.Fatal(err)
				}
			}
			if _, err := c.store.db.Exec(`INSERT INTO `+table+`_data(id, block) VALUES (?, ?)`, ftsStructureRowID, unrefStructure(tc.segs)); err != nil { //nolint:gosec // a constant table name in a test
				t.Fatal(err)
			}
			ins(ftsAveragesRowID) // the two rows every index holds are never reported
			listed := map[int64]bool{}
			for _, s := range tc.segs {
				listed[int64(s)] = true
			}
			var want []int64
			seen := map[int64]bool{}
			for _, id := range tc.rows {
				if seen[id] || id == ftsAveragesRowID || id == ftsStructureRowID {
					continue
				}
				seen[id] = true
				ins(id)
				// the definition, applied to one row: below segment 1 nothing is owned, above it the segment decides
				if id < seg(1) || !listed[id>>ftsSegidShift] {
					want = append(want, id)
				}
			}
			slices.Sort(want)
			rep := &VerifyReport{Problems: []string{}, Notices: []string{}}
			r := &ftsVerify{c: c, ctx: context.Background(), ps: &problemSet{rep: rep, counts: map[string]int{}}, rep: rep, listed: map[string]map[int64]bool{}}
			r.checkUnreferenced(table)
			var got []int64
			for _, p := range rep.Problems {
				m := row.FindStringSubmatch(p)
				if m == nil {
					t.Fatalf("a problem that names no data row: %q", p)
				}
				id, err := strconv.ParseInt(m[1], 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, id)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("reported rows\n got %v\nwant %v", got, want)
			}
		})
	}
}
