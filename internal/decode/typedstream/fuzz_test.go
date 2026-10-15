package typedstream

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

// fuzzMaxInput bounds an input of the fuzz targets; it is above MaxScan plus a header so the
// scan-window rules are reachable.
const fuzzMaxInput = 1 << 17

// fuzzSeeds are the seeds of every target and of the independent-reader test: builder streams
// of every length form, truncations and the hostile cases of the unit tests.
func fuzzSeeds() [][]byte {
	var out [][]byte
	for _, n := range []int{0, 1, 5, 127, 128, 255, 256, 1000, 65535, 65536} {
		out = append(out, buildStream(strings.Repeat("a", n)))
	}
	out = append(out,
		buildStream("changed", mutable()),
		buildStream("é你好\x00\xff", mutable()),
		buildStream("\x08NSString\x01+\x03abc"),
		buildStream("x \x0fNSMutableString\x01+\x02zz"),
		buildStream("abc", rawLen(0x7F)),
		buildStream("abc", rawLen(0x81, 0xFF, 0xFF)),
		buildStream("abc", rawLen(0x82, 0xFF, 0xFF, 0xFF, 0xFF)),
		buildStream("abc", rawLen(0x82, 0x00, 0x00, 0x00, 0x80)),
		buildStream("abc", rawLen(0x83, 3, 0, 0, 0, 0, 0, 0, 0)),
		buildStream("abc", rawLen(0x80)),
		buildStream("abc", rawLen(0x84)),
		buildStream("ignored", withoutString()),
		buildStream("ignored", withoutString(), padding(MaxScan)),
		buildStream("inside", padding(MaxScan-200)),
		buildStream("beyond", padding(MaxScan)),
		lengthCut(),
		markerNoPayload(0),
		markerNoPayload(MaxScan-100),
		append(markerNoPayload(MaxScan), 0x01, '+', 0x01, 'x'),
		windowStream(MaxScan-1, fullString),
		windowStream(MaxScan, fullString),
		windowStream(MaxScan-1, cutString),
		windowStream(MaxScan, cutString),
		windowStream(MaxScan, ""),
		header(),
		[]byte{0x04},
		[]byte("bplist00\x00\x00"),
		[]byte{},
	)
	for _, s := range []string{"truncate me please", "a"} {
		full := buildStream(s)
		for n := 0; n < len(full); n++ {
			out = append(out, full[:n])
		}
	}
	return out
}

func addSeeds(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
}

// typedExtractErr reports whether err wraps a package sentinel and not ErrInternal.
func typedExtractErr(err error) bool {
	if errors.Is(err, ErrInternal) {
		return false
	}
	for _, s := range []error{ErrNotTypedstream, ErrTruncated, ErrLimit, ErrNoBudget} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

type extractRun struct {
	text string
	ok   bool
	err  error
	used int64
}

// extractContract runs call twice on a fresh budget each time and checks the contract.
func extractContract(t *testing.T, b []byte, call func(Budget) (string, bool, error)) {
	t.Helper()
	run := func() extractRun {
		v := parse.NewBudget(64 << 20).View()
		text, ok, err := call(v)
		return extractRun{text, ok, err, v.Used()}
	}
	r := run()
	if r.ok {
		if r.err != nil {
			t.Fatalf("ok with error %v", r.err)
		}
		if len(r.text) > MaxText {
			t.Fatalf("text of %d bytes above MaxText", len(r.text))
		}
		if !bytes.Contains(b, []byte(r.text)) {
			t.Fatalf("text %q is not a sub-slice of the input", r.text)
		}
		if r.used != int64(len(r.text)) {
			t.Fatalf("used %d for a text of %d bytes", r.used, len(r.text))
		}
	} else {
		if r.text != "" {
			t.Fatalf("text %q with ok=false", r.text)
		}
		if r.used != 0 {
			t.Fatalf("budget used %d after a failed call", r.used)
		}
		if r.err != nil && !typedExtractErr(r.err) {
			t.Fatalf("untyped or internal error: %v", r.err)
		}
		// P38/P42: "no string object" is a claim about a full scan window. The independent
		// reader decides it exactly: the claim is made if and only if it sees the whole window
		// and no marker.
		o := oracleRead(b)
		if claim := o.class == "" && !o.ok; (r.err == nil) != claim {
			t.Fatalf("no-string claim = %v, the independent reader says %v (%+v)", r.err == nil, claim, o)
		}
	}
	r2 := run()
	if r2.text != r.text || r2.ok != r.ok || r2.used != r.used || (r.err == nil) != (r2.err == nil) ||
		(r.err != nil && r.err.Error() != r2.err.Error()) {
		t.Fatalf("not deterministic: %+v then %+v", r, r2)
	}
}

func FuzzTypedstream(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		extractContract(t, b, func(bu Budget) (string, bool, error) { return ExtractText(b, bu) })
	})
}

// FuzzTypedstreamNoRecover calls extractCore, so a panic is a failure, not an ErrInternal.
func FuzzTypedstreamNoRecover(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		extractContract(t, b, func(bu Budget) (string, bool, error) { return extractCore(b, bu) })
	})
}
