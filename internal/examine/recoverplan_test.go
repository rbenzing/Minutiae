package examine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func run(off, n int64) evidence.Run { return evidence.Run{Offset: off, Length: n} }

func TestFreeMapContains(t *testing.T) {
	m := newFreeMap([]evidence.Run{run(100, 50), run(200, 10), run(1000, 1)}, false)
	tests := []struct {
		name     string
		off, len int64
		want     bool
	}{
		{"inside", 110, 20, true},
		{"whole run", 100, 50, true},
		{"touching start", 100, 1, true},
		{"touching end", 149, 1, true},
		{"one before start", 99, 2, false},
		{"one past end", 149, 2, false},
		{"starts at end", 150, 1, false},
		{"second run", 200, 10, true},
		{"single byte run", 1000, 1, true},
		{"straddles gap", 140, 70, false},
		{"zero length", 110, 0, false},
		{"negative length", 110, -1, false},
		{"negative offset", -1, 5, false},
		{"offset overflow", math.MaxInt64 - 1, 10, false},
		{"length overflow", 100, math.MaxInt64, false},
		{"far away", math.MaxInt64, 1, false},
		{"before everything", 0, 10, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.contains(tc.off, tc.len); got != tc.want {
				t.Errorf("contains(%d,%d) = %v, want %v", tc.off, tc.len, got, tc.want)
			}
		})
	}
	if newFreeMap(nil, false).contains(0, 1) {
		t.Error("an empty map contains nothing")
	}
}

func TestCutAtFirstNonFree(t *testing.T) {
	free := []evidence.Run{run(100, 50), run(200, 100), run(400, 10)}
	tests := []struct {
		name        string
		unknown     bool
		in          []evidence.Run
		captured    []evidence.Run
		excluded    []evidence.Run
		state       string
		capB, exclB int64
	}{
		{name: "fully free", in: []evidence.Run{run(100, 50), run(200, 100)}, captured: []evidence.Run{run(100, 50), run(200, 100)}, capB: 150},
		{name: "empty input"},
		{
			name: "first run allocated", in: []evidence.Run{run(0, 10), run(100, 50)},
			excluded: []evidence.Run{run(0, 10), run(100, 50)}, state: "allocated", exclB: 60,
		},
		{
			name: "cut in the middle of a run", in: []evidence.Run{run(100, 80)},
			captured: []evidence.Run{run(100, 50)}, excluded: []evidence.Run{run(150, 30)}, state: "allocated", capB: 50, exclB: 30,
		},
		{
			name: "starts free ends allocated after a whole run", in: []evidence.Run{run(100, 50), run(290, 30)},
			captured: []evidence.Run{run(100, 50), run(290, 10)}, excluded: []evidence.Run{run(300, 20)}, state: "allocated", capB: 60, exclB: 20,
		},
		{
			name: "later free runs after a non-free byte are excluded", in: []evidence.Run{run(100, 60), run(200, 100), run(400, 10)},
			captured: []evidence.Run{run(100, 50)}, excluded: []evidence.Run{run(150, 10), run(200, 100), run(400, 10)},
			state: "allocated", capB: 50, exclB: 120,
		},
		{
			name: "free run after an allocated run is never captured", in: []evidence.Run{run(0, 4), run(200, 100)},
			excluded: []evidence.Run{run(0, 4), run(200, 100)}, state: "allocated", exclB: 104,
		},
		{
			name: "adjacent in file order, split in the image", in: []evidence.Run{run(100, 50), run(400, 10), run(200, 5)},
			captured: []evidence.Run{run(100, 50), run(400, 10), run(200, 5)}, capB: 65,
		},
		{
			name: "unknown state", unknown: true, in: []evidence.Run{run(100, 60)},
			captured: []evidence.Run{run(100, 50)}, excluded: []evidence.Run{run(150, 10)}, state: "unknown", capB: 50, exclB: 10,
		},
		{
			name: "huge offset is excluded not wrapped", in: []evidence.Run{run(100, 10), run(math.MaxInt64-5, 100)},
			captured: []evidence.Run{run(100, 10)}, excluded: []evidence.Run{run(math.MaxInt64-5, 100)}, state: "allocated", capB: 10, exclB: 100,
		},
		{
			name: "negative offset is excluded", in: []evidence.Run{run(-1, 10)},
			excluded: []evidence.Run{run(-1, 10)}, state: "allocated", exclB: 10,
		},
		{
			name: "zero length runs carry no bytes", in: []evidence.Run{run(100, 0), run(100, 10), run(5, 0)},
			captured: []evidence.Run{run(100, 10)}, capB: 10,
		},
		{
			name: "excluded byte count saturates", in: []evidence.Run{run(0, 1), run(0, math.MaxInt64), run(0, math.MaxInt64)},
			excluded: []evidence.Run{run(0, 1), run(0, math.MaxInt64), run(0, math.MaxInt64)}, state: "allocated", exclB: math.MaxInt64,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cutAtFirstNonFree(newFreeMap(free, tc.unknown), tc.in)
			if !slices.Equal(got.Captured, tc.captured) || !slices.Equal(got.Excluded, tc.excluded) {
				t.Errorf("captured %v excluded %v; want %v and %v", got.Captured, got.Excluded, tc.captured, tc.excluded)
			}
			if got.State != tc.state || got.CapturedBytes != tc.capB || got.ExcludedBytes != tc.exclB {
				t.Errorf("state %q bytes %d/%d; want %q %d/%d", got.State, got.CapturedBytes, got.ExcludedBytes, tc.state, tc.capB, tc.exclB)
			}
		})
	}
}

// expand lists the byte addresses of runs in order (zero-length runs add none).
func expand(runs []evidence.Run) []int64 {
	var out []int64
	for _, r := range runs {
		for i := range r.Length {
			out = append(out, r.Offset+i)
		}
	}
	return out
}

// bitmapFree builds the merged free runs of a bitmap.
func bitmapFree(bm []bool) []evidence.Run {
	var out []evidence.Run
	for i := 0; i < len(bm); {
		if !bm[i] {
			i++
			continue
		}
		j := i
		for j < len(bm) && bm[j] {
			j++
		}
		out = append(out, run(int64(i), int64(j-i)))
		i = j
	}
	return out
}

// checkCutAgainstBitmap holds the property shared by the property and fuzz tests.
func checkCutAgainstBitmap(t testing.TB, bm []bool, in []evidence.Run, unknown bool) {
	t.Helper()
	isFree := func(a int64) bool { return a >= 0 && a < int64(len(bm)) && bm[a] }
	got := cutAtFirstNonFree(newFreeMap(bitmapFree(bm), unknown), in)
	all := expand(in)
	want := 0
	for want < len(all) && isFree(all[want]) {
		want++
	}
	capt := expand(got.Captured)
	if !slices.Equal(capt, all[:want]) {
		t.Fatalf("captured bytes differ from the longest free prefix (%d bytes): in %v bitmap-free %v got %v", want, in, bitmapFree(bm), got.Captured)
	}
	joined := slices.Concat(capt, expand(got.Excluded))
	if !slices.Equal(joined, all) {
		t.Fatalf("captured+excluded is not the input: in %v got %v + %v", in, got.Captured, got.Excluded)
	}
	if got.CapturedBytes != int64(want) || got.ExcludedBytes != int64(len(all)-want) {
		t.Fatalf("byte counts %d/%d, want %d/%d", got.CapturedBytes, got.ExcludedBytes, want, len(all)-want)
	}
	wantState := ""
	if want < len(all) {
		wantState = "allocated"
		if unknown {
			wantState = "unknown"
		}
	}
	if got.State != wantState {
		t.Fatalf("state %q, want %q", got.State, wantState)
	}
}

func TestCutAtFirstNonFreeMatchesBitmapModel(t *testing.T) {
	const space = 4096
	rng := rand.New(rand.NewSource(20261007)) //nolint:gosec // seeded test data, not security
	for iter := range 2000 {
		bm := make([]bool, space)
		density := rng.Intn(100)
		for i := 0; i < space; {
			n := 1 + rng.Intn(300)
			v := rng.Intn(100) < density
			for j := i; j < i+n && j < space; j++ {
				bm[j] = v
			}
			i += n
		}
		var in []evidence.Run
		for range rng.Intn(7) {
			off := int64(rng.Intn(space + 100))
			if rng.Intn(20) == 0 {
				off = -int64(rng.Intn(3))
			}
			in = append(in, run(off, int64(rng.Intn(400))))
		}
		// half the cases start inside a free run so a real prefix exists
		if fr := bitmapFree(bm); len(fr) > 0 && iter%2 == 0 && len(in) > 0 {
			f := fr[rng.Intn(len(fr))]
			in[0] = run(f.Offset+int64(rng.Intn(int(f.Length))), int64(1+rng.Intn(500)))
		}
		checkCutAgainstBitmap(t, bm, in, iter%3 == 0)
	}
}

func TestOverlapSweep(t *testing.T) {
	many := make([]ownedRuns, 100)
	for i := range many {
		many[i] = ownedRuns{Owner: fmt.Sprintf("o%03d", i), Runs: []evidence.Run{run(0, 100)}}
	}
	got := overlapsOf(many)
	for i, o := range many {
		var want []string
		for j := 0; len(want) < 16; j++ {
			if j != i {
				want = append(want, fmt.Sprintf("o%03d", j))
			}
		}
		if !slices.Equal(got[o.Owner], want) {
			t.Fatalf("%s lists %v, want %v", o.Owner, got[o.Owner], want)
		}
	}
	if again := overlapsOf(many); fmt.Sprint(again) != fmt.Sprint(got) {
		t.Error("the listing is not deterministic")
	}

	tests := []struct {
		name string
		in   []ownedRuns
		want map[string][]string
	}{
		{"disjoint", []ownedRuns{{"a", []evidence.Run{run(0, 10)}}, {"b", []evidence.Run{run(20, 10)}}}, map[string][]string{}},
		{"touching is not overlap", []ownedRuns{{"a", []evidence.Run{run(0, 10)}}, {"b", []evidence.Run{run(10, 10)}}}, map[string][]string{}},
		{
			"one byte",
			[]ownedRuns{{"a", []evidence.Run{run(0, 10)}}, {"b", []evidence.Run{run(9, 10)}}},
			map[string][]string{"a": {"b"}, "b": {"a"}},
		},
		{
			"nested",
			[]ownedRuns{{"a", []evidence.Run{run(0, 100)}}, {"b", []evidence.Run{run(10, 5)}}},
			map[string][]string{"a": {"b"}, "b": {"a"}},
		},
		{
			"three-way",
			[]ownedRuns{{"a", []evidence.Run{run(0, 10)}}, {"b", []evidence.Run{run(5, 10)}}, {"c", []evidence.Run{run(8, 10)}}},
			map[string][]string{"a": {"b", "c"}, "b": {"a", "c"}, "c": {"a", "b"}},
		},
		{
			"chain does not link the ends",
			[]ownedRuns{{"a", []evidence.Run{run(0, 10)}}, {"b", []evidence.Run{run(5, 10)}}, {"c", []evidence.Run{run(12, 10)}}},
			map[string][]string{"a": {"b"}, "b": {"a", "c"}, "c": {"b"}},
		},
		{"same owner twice is exempt", []ownedRuns{{"a", []evidence.Run{run(0, 10), run(5, 10)}}}, map[string][]string{}},
		{"same owner listed twice is exempt", []ownedRuns{{"a", []evidence.Run{run(0, 10)}}, {"a", []evidence.Run{run(5, 10)}}}, map[string][]string{}},
		{
			"runs apart in one owner",
			[]ownedRuns{{"a", []evidence.Run{run(0, 5), run(100, 5)}}, {"b", []evidence.Run{run(102, 1)}}},
			map[string][]string{"a": {"b"}, "b": {"a"}},
		},
		{
			"zero length and invalid runs are ignored",
			[]ownedRuns{{"a", []evidence.Run{run(0, 0), run(-5, 10), run(math.MaxInt64, 5)}}, {"b", []evidence.Run{run(0, 10), run(math.MaxInt64, 5)}}},
			map[string][]string{},
		},
		{"empty", nil, map[string][]string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := overlapsOf(tc.in)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOverlapIsSymmetric(t *testing.T) {
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // seeded test data, not security
	for range 300 {
		n := 2 + rng.Intn(8) // at most 9 owners: no list reaches the cap of 16
		items := make([]ownedRuns, n)
		for i := range items {
			items[i].Owner = fmt.Sprintf("x%d", i)
			for range 1 + rng.Intn(3) {
				items[i].Runs = append(items[i].Runs, run(int64(rng.Intn(200)), int64(rng.Intn(40))))
			}
		}
		got := overlapsOf(items)
		for a, others := range got {
			if !slices.IsSorted(others) || slices.Contains(others, a) {
				t.Fatalf("%s lists %v", a, others)
			}
			for _, b := range others {
				if !slices.Contains(got[b], a) {
					t.Fatalf("%s lists %s but not the reverse: %v", a, b, got)
				}
			}
		}
		// brute force oracle
		for i := range items {
			var want []string
			for j := range items {
				if i != j && sharesByte(items[i].Runs, items[j].Runs) {
					want = append(want, items[j].Owner)
				}
			}
			slices.Sort(want)
			if !slices.Equal(got[items[i].Owner], want) {
				t.Fatalf("%s: got %v, want %v (%v)", items[i].Owner, got[items[i].Owner], want, items)
			}
		}
	}
}

func sharesByte(a, b []evidence.Run) bool {
	for _, x := range a {
		for _, y := range b {
			if x.Length > 0 && y.Length > 0 && x.Offset < y.Offset+y.Length && y.Offset < x.Offset+x.Length {
				return true
			}
		}
	}
	return false
}

// specTable is a literal copy of the spec 3.3 table for class deleted-file.
var specTable = map[string]int{
	"ext-inode-intact":   80,
	"f2fs-node-scan":     75,
	"exfat-nofatchain":   75,
	"ext4-journal-inode": 65,
	"ext4-extent-leaf":   55,
	"fat-contiguous":     55,
	"exfat-contiguous":   55,
}

func TestConfidenceRules(t *testing.T) {
	if len(methodBase) != len(specTable) {
		t.Fatalf("%d methods, the spec has %d", len(methodBase), len(specTable))
	}
	for m, want := range specTable {
		got, ok := confidenceFor(m, false, false)
		if !ok || got != want {
			t.Errorf("%s = %d,%v; want %d", m, got, ok, want)
		}
	}
	for _, m := range []string{"", "fat-unknown", "carve-validated", "FAT-CONTIGUOUS"} {
		if v, ok := confidenceFor(m, false, false); ok || v != 0 {
			t.Errorf("unknown method %q = %d,%v; want 0,false", m, v, ok)
		}
	}
	if capOverlap != 30 || capCut != 20 {
		t.Errorf("caps %d and %d, want 30 and 20", capOverlap, capCut)
	}
}

func TestConfidenceCaps(t *testing.T) {
	for m, base := range specTable {
		for _, cut := range []bool{false, true} {
			for _, ov := range []bool{false, true} {
				want := base
				if cut {
					want = min(want, 20)
				}
				if ov {
					want = min(want, 30)
				}
				got, ok := confidenceFor(m, cut, ov)
				if !ok || got != want || got > 80 {
					t.Errorf("%s cut=%v overlap=%v = %d,%v; want %d", m, cut, ov, got, ok, want)
				}
			}
		}
	}
	if v, _ := confidenceFor("ext-inode-intact", true, true); v != 20 {
		t.Errorf("both caps = %d, want 20", v)
	}
}

func TestMethodsAreAllowedByClassTable(t *testing.T) {
	ci, ok := evidence.LookupClass("deleted-file")
	if !ok {
		t.Fatal("no deleted-file class")
	}
	for m := range methodBase {
		okPrefix := false
		for _, p := range ci.MethodPrefixes {
			if strings.HasPrefix(m, p) {
				okPrefix = true
			}
		}
		if !okPrefix {
			t.Errorf("method %q matches no prefix of %v", m, ci.MethodPrefixes)
		}
		if base := methodBase[m]; base > ci.MaxConfidence {
			t.Errorf("method %q base %d is above the class ceiling %d", m, base, ci.MaxConfidence)
		}
	}
}

// countReader counts ReadAt calls.
type countReader struct {
	r     io.ReaderAt
	calls int
	err   error
}

func (c *countReader) ReadAt(p []byte, off int64) (int, error) {
	c.calls++
	if c.err != nil {
		return 0, c.err
	}
	return c.r.ReadAt(p, off)
}

func TestUniformScan(t *testing.T) {
	ctx := context.Background()
	t.Run("all zero", func(t *testing.T) {
		u, f, err := uniformScan(ctx, bytes.NewReader(make([]byte, 1<<20)), []evidence.Run{run(0, 1<<20)})
		if err != nil || !u || f != 0 {
			t.Fatalf("%v %#x %v", u, f, err)
		}
	})
	t.Run("all ff over two runs", func(t *testing.T) {
		u, f, err := uniformScan(ctx, bytes.NewReader(bytes.Repeat([]byte{0xFF}, 300000)), []evidence.Run{run(0, 100000), run(150000, 140000)})
		if err != nil || !u || f != 0xFF {
			t.Fatalf("%v %#x %v", u, f, err)
		}
	})
	data := make([]byte, 300000)
	for _, pos := range []int{0, 99999, 100000, 250000} {
		t.Run(fmt.Sprintf("differs at %d", pos), func(t *testing.T) {
			d := slices.Clone(data)
			d[pos] = 1
			u, _, err := uniformScan(ctx, bytes.NewReader(d), []evidence.Run{run(0, 100000), run(100000, 200000)})
			if err != nil || u {
				t.Fatalf("uniform=%v err=%v", u, err)
			}
		})
	}
	t.Run("differing byte in the second run", func(t *testing.T) {
		d := slices.Clone(data)
		d[200000] = 9
		u, _, err := uniformScan(ctx, bytes.NewReader(d), []evidence.Run{run(0, 100), run(200000, 10)})
		if err != nil || u {
			t.Fatalf("uniform=%v err=%v", u, err)
		}
	})
	t.Run("fill differs between runs", func(t *testing.T) {
		d := make([]byte, 20)
		for i := 10; i < 20; i++ {
			d[i] = 0xFF
		}
		u, _, err := uniformScan(ctx, bytes.NewReader(d), []evidence.Run{run(0, 10), run(10, 10)})
		if err != nil || u {
			t.Fatalf("uniform=%v err=%v", u, err)
		}
	})
	t.Run("empty and zero-length lists are not uniform", func(t *testing.T) {
		for _, runs := range [][]evidence.Run{nil, {}, {run(0, 0)}} {
			u, _, err := uniformScan(ctx, bytes.NewReader(data), runs)
			if err != nil || u {
				t.Fatalf("%v: uniform=%v err=%v", runs, u, err)
			}
		}
	})
	t.Run("early exit reads one chunk", func(t *testing.T) {
		d := make([]byte, 64<<20)
		d[10] = 1
		cr := &countReader{r: bytes.NewReader(d)}
		u, _, err := uniformScan(ctx, cr, []evidence.Run{run(0, int64(len(d)))})
		if err != nil || u || cr.calls != 1 {
			t.Fatalf("uniform=%v err=%v calls=%d", u, err, cr.calls)
		}
	})
	t.Run("read error", func(t *testing.T) {
		boom := errors.New("boom")
		_, _, err := uniformScan(ctx, &countReader{err: boom}, []evidence.Run{run(0, 10)})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		cr := &countReader{r: bytes.NewReader(data)}
		_, _, err := uniformScan(c, cr, []evidence.Run{run(0, 10)})
		if !errors.Is(err, context.Canceled) || cr.calls != 0 {
			t.Fatalf("err = %v calls=%d", err, cr.calls)
		}
	})
	t.Run("runs past the end are an error", func(t *testing.T) {
		u, _, err := uniformScan(ctx, bytes.NewReader(make([]byte, 100)), []evidence.Run{run(50, 100)})
		if err == nil || u {
			t.Fatalf("uniform=%v err=%v", u, err)
		}
	})
	t.Run("invalid runs are an error", func(t *testing.T) {
		for _, r := range []evidence.Run{run(-1, 5), run(0, -1), run(math.MaxInt64, 5)} {
			u, _, err := uniformScan(ctx, bytes.NewReader(data), []evidence.Run{r})
			if err == nil || u {
				t.Fatalf("%v: uniform=%v err=%v", r, u, err)
			}
		}
	})
}
