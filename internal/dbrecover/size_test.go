package dbrecover

import (
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// TestRecordBytesNeverOverflowsOnAnUnmeasurablePayload (J113): a payload that
// cannot be measured must not turn into a sentinel that wraps the byte total
// negative and lets the max-bytes check pass.
func TestRecordBytesNeverOverflowsOnAnUnmeasurablePayload(t *testing.T) {
	r := records.Record{Summary: "x", Payload: map[string]any{"bad": math.NaN()}}
	n, err := recordBytes(r)
	if !errors.Is(err, errPayloadSize) || n != 0 {
		t.Errorf("recordBytes = %d, %v for an unmeasurable payload: want 0 and errPayloadSize", n, err)
	}
	if n, err := encodedSize(map[string]any{"bad": math.Inf(1)}); !errors.Is(err, errPayloadSize) || n != 0 {
		t.Errorf("encodedSize = %d, %v, want 0 and errPayloadSize", n, err)
	}
	ok, err := recordBytes(records.Record{Summary: "x", Payload: map[string]any{"a": int64(1)}})
	if err != nil || ok != 1+int64(len(`{"a":1}`)) {
		t.Errorf("recordBytes of a plain record = %d, %v", ok, err)
	}
}

func mustSize(m map[string]any) int64 {
	n, err := encodedSize(m)
	if err != nil {
		panic(err)
	}
	return n
}

func mustColumns(vals []sqlitefile.Value, allowance int64) []any {
	cols, err := encodeColumns(vals, allowance)
	if err != nil {
		panic(err)
	}
	return cols
}
