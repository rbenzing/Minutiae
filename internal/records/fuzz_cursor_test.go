package records_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

// FuzzCursor: decoding never panics, a failure is ErrBadCursor and a success
// re-encodes to exactly the input. Run: go test -fuzz FuzzCursor -fuzztime 60s
// -fuzzminimizetime 0 ./internal/records
func FuzzCursor(f *testing.F) {
	for _, s := range []string{
		"", "v1.", "v1.e30", "garbage", "v2.abc",
		records.Cursor{ID: 7, TS: 5}.Encode(), records.Cursor{D: 1, N: 1, ID: 1}.Encode(),
		records.Cursor{D: 1, TS: -9, ID: 1 << 40}.Encode(),
		"v1." + b64(`{"d":0,"n":0,"ts":1e30,"id":1}`), "v1." + b64(`{"d":0,"n":0,"ts":1,"id":-1}`),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := records.DecodeCursor(s)
		if err != nil {
			if !errors.Is(err, records.ErrBadCursor) {
				t.Fatalf("decode %q: %v is not ErrBadCursor", s, err)
			}
			return
		}
		if got := c.Encode(); got != s {
			t.Fatalf("decoded %q re-encodes to %q", s, got)
		}
	})
}
