package records_test

import (
	"encoding/base64"
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestCursorRoundTrip(t *testing.T) {
	for _, c := range []records.Cursor{
		{D: 0, N: 0, TS: 0, ID: 1},
		{D: 1, N: 0, TS: -5, ID: 7},
		{D: 0, N: 1, TS: 0, ID: 99},
		{D: 1, N: 1, TS: 0, ID: math.MaxInt64},
		{D: 0, N: 0, TS: math.MinInt64, ID: 3},
		{D: 1, N: 0, TS: math.MaxInt64, ID: 3},
	} {
		s := c.Encode()
		got, err := records.DecodeCursor(s)
		if err != nil || got != c {
			t.Errorf("%+v -> %q -> %+v, %v", c, s, got, err)
		}
	}
}

func TestCursorWireFormIsVersioned(t *testing.T) {
	s := records.Cursor{TS: 12, ID: 3}.Encode()
	if len(s) < 4 || s[:3] != "v1." {
		t.Fatalf("cursor %q does not start with v1.", s)
	}
	if _, err := records.DecodeCursor("v2." + s[3:]); !errors.Is(err, records.ErrBadCursor) {
		t.Fatalf("v2: %v", err)
	}
}
