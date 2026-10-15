package evidence_test

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func TestReadUnallocRunMap(t *testing.T) {
	good := `{"offset":0,"length":10,"image_offset":100}` + "\n" +
		`{"offset":10,"length":5,"image_offset":50}` + "\n" +
		`{"offset":15,"length":1,"image_offset":0}` + "\n"
	got, err := evidence.ReadUnallocRunMap(strings.NewReader(good))
	want := []evidence.UnallocRun{{0, 10, 100}, {10, 5, 50}, {15, 1, 0}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
	if got, err := evidence.ReadUnallocRunMap(strings.NewReader("")); err != nil || len(got) != 0 {
		t.Errorf("empty file: %v, %v", got, err)
	}
	if _, err := evidence.ReadUnallocRunMap(strings.NewReader(strings.TrimSuffix(good, "\n"))); err != nil {
		t.Errorf("no final newline: %v", err)
	}
	line := func(o, l, i int64) string {
		return fmt.Sprintf(`{"offset":%d,"length":%d,"image_offset":%d}`+"\n", o, l, i)
	}
	var sb strings.Builder
	for i := range evidence.MaxRecoveredRuns + 1 {
		sb.WriteString(line(int64(i), 1, int64(i)))
	}
	many := sb.String()
	bad := map[string]string{
		"run-shaped line":    `{"offset":0,"length":10}` + "\n",
		"unknown field":      `{"offset":0,"length":10,"image_offset":0,"x":1}` + "\n",
		"missing offset":     `{"length":10,"image_offset":0}` + "\n",
		"nonzero first":      line(5, 10, 0),
		"gap":                line(0, 10, 0) + line(11, 1, 0),
		"overlap":            line(0, 10, 0) + line(9, 1, 0),
		"zero length":        line(0, 0, 0),
		"negative length":    line(0, -1, 0),
		"negative image off": line(0, 1, -1),
		"offset overflow":    line(0, 10, 0) + line(10, math.MaxInt64, 0),
		"image overflow":     line(0, 10, math.MaxInt64),
		"long line":          `{"offset":0,"length":1,"image_offset":0,"` + strings.Repeat("a", 240) + `":1}` + "\n",
		"two values":         `{"offset":0,"length":1,"image_offset":0} {"offset":1,"length":1,"image_offset":0}` + "\n",
		"trailing garbage":   `{"offset":0,"length":1,"image_offset":0}x` + "\n",
		"too many lines":     many,
	}
	for name, in := range bad {
		if _, err := evidence.ReadUnallocRunMap(strings.NewReader(in)); !errors.Is(err, evidence.ErrIntegrity) {
			t.Errorf("%s: err %v, want ErrIntegrity", name, err)
		}
	}
}

func TestUnallocRunsAsRuns(t *testing.T) {
	got := evidence.UnallocRunsAsRuns([]evidence.UnallocRun{{0, 10, 100}, {10, 5, 50}})
	want := []evidence.Run{{Offset: 100, Length: 10}, {Offset: 50, Length: 5}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestReadUnallocRunMapRefusesDuplicateAndCaseVariantKeys(t *testing.T) {
	bad := map[string]string{
		"duplicate offset":       `{"offset":0,"offset":0,"length":10,"image_offset":0}` + "\n",
		"duplicate hidden value": `{"offset":0,"length":10,"image_offset":5,"image_offset":0}` + "\n",
		"duplicate length":       `{"offset":0,"length":10,"length":20,"image_offset":0}` + "\n",
		"capitalised offset":     `{"Offset":0,"length":10,"image_offset":0}` + "\n",
		"upper-case length":      `{"offset":0,"LENGTH":10,"image_offset":0}` + "\n",
		"mixed image_offset":     `{"offset":0,"length":10,"Image_Offset":0}` + "\n",
		"duplicate in line 2":    `{"offset":0,"length":10,"image_offset":0}` + "\n" + `{"offset":10,"offset":10,"length":1,"image_offset":0}` + "\n",
	}
	for name, in := range bad {
		if _, err := evidence.ReadUnallocRunMap(strings.NewReader(in)); !errors.Is(err, evidence.ErrIntegrity) {
			t.Errorf("%s: err %v, want ErrIntegrity", name, err)
		}
	}
}

func TestReadUnallocRunMapBoundaries(t *testing.T) {
	const base = `{"offset":0,"length":1,"image_offset":0}`
	pad := func(n int) string { return base + strings.Repeat(" ", n-len(base)) }
	if got, err := evidence.ReadUnallocRunMap(strings.NewReader(pad(256) + "\n")); err != nil || len(got) != 1 {
		t.Errorf("256-byte line: %v, %v", got, err)
	}
	if _, err := evidence.ReadUnallocRunMap(strings.NewReader(pad(257) + "\n")); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("257-byte line: err %v, want ErrIntegrity", err)
	}
	const length = 4096
	ok := fmt.Sprintf(`{"offset":0,"length":%d,"image_offset":%d}`+"\n", length, int64(math.MaxInt64-length))
	if got, err := evidence.ReadUnallocRunMap(strings.NewReader(ok)); err != nil || len(got) != 1 {
		t.Errorf("image_offset MaxInt64-length: %v, %v", got, err)
	}
	over := fmt.Sprintf(`{"offset":0,"length":%d,"image_offset":%d}`+"\n", length, int64(math.MaxInt64-length+1))
	if _, err := evidence.ReadUnallocRunMap(strings.NewReader(over)); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("image_offset MaxInt64-length+1: err %v, want ErrIntegrity", err)
	}
}
