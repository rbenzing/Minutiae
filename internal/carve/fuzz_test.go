package carve

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fuzzValidateBudget is the validator read budget of a fuzz scan: small, so an
// input that makes validators re-read is cut by the budget, not by the clock.
const fuzzValidateBudget = 4 << 20

// fuzzCarveRun is the body of FuzzCarve: data is split at len/3 into two
// adjacent runs, scanned with the built-in set, and the hostile-input
// guarantees are checked. It returns the families that were accepted.
func fuzzCarveRun(t testing.TB, data []byte, align uint16, nested bool) map[string]int {
	set := Builtin()
	maxLen := map[string]int64{}
	for _, ty := range set.Types {
		maxLen[ty.Name] = ty.MaxLen
	}
	cut := int64(len(data) / 3)
	runs := []Run{{0, cut}, {cut, int64(len(data)) - cut}}
	if cut == 0 {
		runs = runs[1:]
	}
	families := map[string]int{}
	st, err := Scan(context.Background(), bytes.NewReader(data), runs, set,
		Options{Align: int64(align), Nested: nested, ValidateBudget: fuzzValidateBudget},
		func(h Hit) error {
			families[h.Family]++
			if h.Run < 0 || h.Run >= len(runs) {
				t.Fatalf("hit %+v names no run", h)
			}
			r := runs[h.Run]
			if h.Offset < r.Offset || h.Length < 1 || h.Offset+h.Length > r.Offset+r.Length {
				t.Fatalf("hit %s at %d length %d leaves run %+v", h.Family, h.Offset, h.Length, r)
			}
			if h.Length > maxLen[h.Family] {
				t.Fatalf("hit %s length %d exceeds MaxLen %d", h.Family, h.Length, maxLen[h.Family])
			}
			return nil
		})
	var pe *PanicError
	if errors.As(err, &pe) {
		t.Fatalf("scan panicked: %v", pe)
	}
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if st.OutsideReads != 0 {
		t.Fatalf("OutsideReads = %d, want 0", st.OutsideReads)
	}
	if st.ValidateBytes > fuzzValidateBudget+64<<10 {
		t.Fatalf("ValidateBytes = %d exceeds the budget %d + 64 KiB", st.ValidateBytes, int64(fuzzValidateBudget))
	}
	return families
}

// FuzzCarve scans arbitrary bytes, split into two runs, with the built-in set.
func FuzzCarve(f *testing.F) {
	for _, obj := range builderCorpus()[:3] {
		f.Add(fuzzSeedData(obj.data), uint16(0), false)
	}
	f.Fuzz(func(t *testing.T, data []byte, align uint16, nested bool) {
		fuzzCarveRun(t, data, align, nested)
	})
}

// fuzzSeedData places obj so that the split at len/3 falls exactly at the
// object's first byte: the second run opens with the whole object.
func fuzzSeedData(obj []byte) []byte {
	pad := bytes.Repeat(fillPattern, len(obj)/(2*len(fillPattern))+1)[:len(obj)/2]
	return append(pad, obj...)
}

// parseFuzzSeed reads a corpus file of the go test fuzz v1 encoding for the
// (data []byte, align uint16, nested bool) signature of FuzzCarve.
func parseFuzzSeed(t testing.TB, path string) (data []byte, align uint16, nested bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n"), "\n")
	if len(lines) != 4 || lines[0] != "go test fuzz v1" {
		t.Fatalf("%s: not a three-value fuzz corpus file", path)
	}
	body, ok := strings.CutPrefix(lines[1], "[]byte(")
	if !ok || !strings.HasSuffix(body, ")") {
		t.Fatalf("%s: line 2 is not a []byte value", path)
	}
	s, err := strconv.Unquote(strings.TrimSuffix(body, ")"))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	a, ok := strings.CutPrefix(lines[2], "uint16(")
	if !ok {
		t.Fatalf("%s: line 3 is not a uint16", path)
	}
	av, err := strconv.ParseUint(strings.TrimSuffix(a, ")"), 10, 16)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	n, ok := strings.CutPrefix(lines[3], "bool(")
	if !ok {
		t.Fatalf("%s: line 4 is not a bool", path)
	}
	nv, err := strconv.ParseBool(strings.TrimSuffix(n, ")"))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return []byte(s), uint16(av), nv
}

// TestFuzzCarveSeedsRun: the committed seeds (builder outputs and the real
// objects, each at most 64 KB) pass the fuzz body under plain go test, and
// between them they make every one of the twelve families be accepted, so the
// corpus really starts the fuzzer inside every validator.
func TestFuzzCarveSeedsRun(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "fuzz", "FuzzCarve", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 40 {
		t.Fatalf("%d committed seeds, want at least 40", len(files))
	}
	total := map[string]int{}
	for _, f := range files {
		data, align, nested := parseFuzzSeed(t, f)
		if len(data) > 64<<10 {
			t.Errorf("%s: seed of %d bytes exceeds 64 KB", f, len(data))
		}
		t.Run(filepath.Base(f), func(t *testing.T) {
			for fam, n := range fuzzCarveRun(t, data, align, nested) {
				total[fam] += n
			}
		})
	}
	for _, ty := range Builtin().Types {
		if total[ty.Name] == 0 {
			t.Errorf("no committed seed makes family %q accepted", ty.Name)
		}
	}
}
