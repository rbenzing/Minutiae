package records_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
)

// FuzzValidateRecord feeds arbitrary strings, numbers, times and byte ranges to
// the write-time validation: it never panics, and an accepted record's
// canonical payload is a fixed point (decoding it and encoding again gives the
// same bytes).
func FuzzValidateRecord(f *testing.F) {
	f.Add("event", "summary", "body", "/p", "sqlite:t=a", "k", "v", "12345678901234567890", int64(1700000000), int64(123), "utc", 0, int64(0), int64(10), int64(100), "")
	f.Add("message", "a\x00b", "\xff", "p", "nolocator", "\xff", "é€😀", "1e400", int64(-1), int64(-5), "local-offset", 1440, int64(-1), int64(-1), int64(0), "carve")
	f.Add("nope", "", "", "", "", "", "", "abc", int64(math.MaxInt64), int64(math.MaxInt64), "gps", math.MaxInt32, int64(math.MaxInt64), int64(math.MaxInt64), int64(5), "x")
	f.Add("note", "line\nbreak", "b", "path", "json:ptr=/a", "k\"\\", "<>& ", "-0.5E+3", int64(4102444800), int64(0), "local-unknown", 0, int64(5), int64(5), int64(10), "free-page")
	art := records.ArtifactInfo{ID: "a1", SHA256: strings.Repeat("0", 64), Size: 100}
	f.Fuzz(func(t *testing.T, typ, summary, body, path, locator, key, val, num string, secs, nanos int64, basis string, off int, rOff, rLen, size int64, recovery string) {
		a := art
		a.Size = size
		r := records.Record{
			Type: typ, ArtifactID: "a1", SourcePath: path, Locator: locator,
			Range:    &records.Range{Offset: rOff, Length: rLen},
			Time:     &records.Time{T: time.Unix(secs, nanos), Basis: records.Basis(basis), OffsetMin: off},
			Deleted:  recovery != "" || len(summary)%2 == 0,
			Recovery: recovery,
			Summary:  summary, Body: body,
			Payload: map[string]any{key: val, "n": json.Number(num), "arr": []any{val, json.Number(num), nil}, "ts_note": val},
		}
		if len(body)%3 == 0 {
			r.TimeEnd = r.Time
			r.Times = []records.NamedTime{{Kind: key, Time: *r.Time}, {Kind: "mtime", Time: *r.Time}}
		}
		p, err := records.Prepare(r, a)
		if err != nil {
			return
		}
		row := p.Row()
		var back map[string]any
		dec := json.NewDecoder(strings.NewReader(row.Payload))
		dec.UseNumber()
		if err := dec.Decode(&back); err != nil {
			t.Fatalf("accepted record has a payload that does not decode: %v\n%s", err, row.Payload)
		}
		again, err := records.CanonicalPayload(back)
		if err != nil || again != row.Payload {
			t.Fatalf("canonical payload is not a fixed point:\n%s\n%s (%v)", row.Payload, again, err)
		}
	})
}
