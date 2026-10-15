package evidence

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"testing/iotest"
)

// FA-1: a line that does not name both offset and length is corrupt; a missing key is never read as 0.
func TestReadRunLinesRequiresBothKeys(t *testing.T) {
	for name, in := range map[string]string{
		"empty object":   "{}\n",
		"no offset":      `{"length":4096}` + "\n",
		"no length":      `{"offset":4096}` + "\n",
		"null offset":    `{"offset":null,"length":5}` + "\n",
		"null length":    `{"offset":5,"length":null}` + "\n",
		"both null":      `{"offset":null,"length":null}` + "\n",
		"missing line 2": `{"offset":0,"length":1}` + "\n" + `{"length":1}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := readRunLines(strings.NewReader(in))
			var fe *runsFormatError
			if !errors.As(err, &fe) {
				t.Fatalf("got %v, err %v; want a typed format error", got, err)
			}
		})
	}
}

// E55: the streaming reader keeps every bound of the line-by-line reader.
func TestReadRunLinesStreamKeepsItsBounds(t *testing.T) {
	good := `{"offset":0,"length":1}` + "\n"
	bad := map[string]string{
		"blank line":            good + "\n",
		"leading blank line":    "\n" + good,
		"blank line mid":        good + "\n" + good,
		"spaces-only line":      good + "   \n" + good,
		"trailing spaces EOF":   good + "   ",
		"two values on a line":  `{"offset":0,"length":1}{"offset":0,"length":1}` + "\n",
		"value split on lines":  `{"offset":0,` + "\n" + `"length":1}` + "\n",
		"split after key":       `{"offset":` + "\n" + `0,"length":1}` + "\n",
		"unterminated object":   `{"offset":0,"length":1`,
		"unknown field":         `{"offset":0,"length":1,"x":2}` + "\n",
		"nested unknown":        `{"offset":0,"length":1,"x":{"a":[1]}}` + "\n",
		"duplicate offset":      `{"offset":0,"offset":5,"length":1}` + "\n",
		"case variant":          `{"Offset":0,"length":1}` + "\n",
		"string number":         `{"offset":"0","length":1}` + "\n",
		"float number":          `{"offset":0.5,"length":1}` + "\n",
		"exponent number":       `{"offset":1e3,"length":1}` + "\n",
		"overflowing number":    `{"offset":9223372036854775808,"length":1}` + "\n",
		"array":                 "[0,1]\n",
		"not json":              "nope\n",
		"line of 257 bytes":     `{"offset":0,"length":1}` + strings.Repeat(" ", 257-23) + "\n",
		"unterminated 600 byte": strings.Repeat(" ", 600),
		"too many lines":        strings.Repeat(good, MaxRecoveredRuns+1),
	}
	for name, in := range bad {
		t.Run(name, func(t *testing.T) {
			got, err := readRunLines(strings.NewReader(in))
			var fe *runsFormatError
			if !errors.As(err, &fe) {
				t.Fatalf("got %d runs, err %v; want a typed format error", len(got), err)
			}
		})
	}
	okCases := map[string]struct {
		in   string
		want []Run
	}{
		"empty input":           {"", nil},
		"one line":              {good, []Run{{0, 1}}},
		"no final newline":      {strings.TrimSuffix(good, "\n"), []Run{{0, 1}}},
		"trailing space":        {`{"offset":0,"length":1}  ` + "\r\n", []Run{{0, 1}}},
		"line of 256 bytes":     {`{"offset":0,"length":1}` + strings.Repeat(" ", 256-23) + "\n", []Run{{0, 1}}},
		"key order":             {`{"length":3,"offset":2}` + "\n", []Run{{2, 3}}},
		"hole":                  {`{"offset":-1,"length":4}` + "\n", []Run{{-1, 4}}},
		"exact beyond 2^53":     {`{"offset":9007199254740993,"length":1}` + "\n", []Run{{9007199254740993, 1}}},
		"max int64":             {fmt.Sprintf(`{"offset":%d,"length":%d}`+"\n", int64(math.MaxInt64), int64(math.MaxInt64)), []Run{{math.MaxInt64, math.MaxInt64}}},
		"two lines":             {good + `{"offset":5,"length":6}` + "\n", []Run{{0, 1}, {5, 6}}},
		"exactly the run count": {strings.Repeat(good, MaxRecoveredRuns), nil},
	}
	for name, k := range okCases {
		t.Run(name, func(t *testing.T) {
			got, err := readRunLines(strings.NewReader(k.in))
			if err != nil {
				t.Fatal(err)
			}
			if k.want == nil && name == "exactly the run count" {
				if len(got) != MaxRecoveredRuns {
					t.Fatalf("%d runs", len(got))
				}
				return
			}
			if len(got) != len(k.want) {
				t.Fatalf("got %v, want %v", got, k.want)
			}
			for i := range got {
				if got[i] != k.want[i] {
					t.Fatalf("got %v, want %v", got, k.want)
				}
			}
		})
	}
}

// FA-2 (E36): a read error is not a format error, and it is returned as it is.
func TestReadRunLinesPassesAReadErrorThrough(t *testing.T) {
	boom := errors.New("test: disk read failed")
	in := io.MultiReader(strings.NewReader(`{"offset":0,"length":1}`+"\n"), iotest.ErrReader(boom))
	_, err := readRunLines(in)
	var fe *runsFormatError
	if !errors.Is(err, boom) || errors.As(err, &fe) {
		t.Fatalf("err %v, want the read error and no format error", err)
	}
	// the same inside a line
	in = io.MultiReader(strings.NewReader(`{"offset":0,`), iotest.ErrReader(boom))
	_, err = readRunLines(in)
	if !errors.Is(err, boom) || errors.As(err, &fe) {
		t.Fatalf("mid-line: err %v, want the read error and no format error", err)
	}
}

// FA-2: only a format error is a runs-sidecar integrity problem; an I/O error is not.
func TestSidecarReadErrorClassification(t *testing.T) {
	boom := errors.New("test: EIO")
	if err := sidecarReadError("S1", boom); errors.Is(err, ErrIntegrity) || !errors.Is(err, boom) {
		t.Fatalf("I/O error became %v", err)
	}
	if err := sidecarReadError("S1", &runsFormatError{msg: "line 3: bad"}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("format error became %v", err)
	}
	if err := sidecarReadError("S1", unallocBad("line 3: bad")); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unalloc format error became %v", err)
	}
}
