package dbrecover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func realPayload(bits uint64) map[string]any {
	p := validPayload()
	p["columns"] = []any{map[string]any{
		"i": int64(0), "type": "real",
		"float_bits": fmt.Sprintf("%016x", bits),
		"value":      strconv.FormatFloat(math.Float64frombits(bits), 'g', -1, 64),
	}}
	return p
}

var realCases = map[string]uint64{
	"1.5":         math.Float64bits(1.5),
	"-0":          math.Float64bits(math.Copysign(0, -1)),
	"subnormal":   1,
	"MaxFloat64":  math.Float64bits(math.MaxFloat64),
	"+Inf":        math.Float64bits(math.Inf(1)),
	"-Inf":        math.Float64bits(math.Inf(-1)),
	"NaN payload": 0x7ff8_0000_dead_beef,
}

func TestRealColumnRequiresWellFormedFloatBits(t *testing.T) {
	for name, bits := range realCases {
		if err := validateDBRow(realPayload(bits)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		p := realPayload(bits)
		delete(p["columns"].([]any)[0].(map[string]any), "value") // value absent is fine
		if err := validateDBRow(p); err != nil {
			t.Errorf("%s without value: %v", name, err)
		}
	}
	col := func(m map[string]any) map[string]any {
		p := validPayload()
		m["i"], m["type"] = int64(0), "real"
		p["columns"] = []any{m}
		return p
	}
	for name, m := range map[string]map[string]any{
		"no float_bits":     {"value": "1"},
		"short bits":        {"float_bits": "3ff"},
		"long bits":         {"float_bits": "3ff00000000000000"},
		"upper-case bits":   {"float_bits": "3FF0000000000000"},
		"non-hex bits":      {"float_bits": "3ff000000000000g"},
		"bits not string":   {"float_bits": int64(1)},
		"float64 value":     {"float_bits": "3ff0000000000000", "value": 1.0},
		"json.Number value": {"float_bits": "3ff0000000000000", "value": json.Number("1")},
		"value disagrees":   {"float_bits": "3ff0000000000000", "value": "2"},
		"value not canon":   {"float_bits": "3ff0000000000000", "value": "1.0"},
	} {
		if err := validateDBRow(col(m)); err == nil || !errors.Is(err, records.ErrInvalidPayload) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// float_bits belongs to real columns only
	p := validPayload()
	p["columns"] = []any{map[string]any{"i": int64(0), "type": "integer", "value": int64(1), "float_bits": "3ff0000000000000"}}
	if validateDBRow(p) == nil {
		t.Error("float_bits on an integer column accepted")
	}
	// an unread or omitted real carries no bits
	p = validPayload()
	p["columns"] = []any{
		map[string]any{"i": int64(0), "type": "real", "unread": true},
		map[string]any{"i": int64(1), "type": "real", "omitted": true},
	}
	if err := validateDBRow(p); err != nil {
		t.Errorf("unread/omitted real: %v", err)
	}
}

func TestRealColumnRoundTripsThroughCanonicalPayload(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", []byte("x"))
	var recs []records.Record
	names := make([]string, 0, len(realCases))
	for name, bits := range realCases {
		names = append(names, name)
		recs = append(recs, records.Record{
			Type: TypeName, ArtifactID: art.ID, Summary: name, Deleted: true, Recovery: "sqlite-freelist",
			Payload: realPayload(bits),
		})
	}
	res := recordstest.Ingest(t, c, records.Parser{Name: "t", Version: "1"}, []string{art.ID}, recs)
	if res.Records != int64(len(recs)) {
		t.Fatalf("stored %d of %d records", res.Records, len(recs))
	}
	rd, err := records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		bits := realCases[name]
		full, err := rd.Get(context.Background(), res.FirstID+int64(i))
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Columns []struct {
				FloatBits string `json:"float_bits"`
				Value     string `json:"value"`
			} `json:"columns"`
		}
		if err := json.Unmarshal(full.Payload, &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		back, err := strconv.ParseUint(got.Columns[0].FloatBits, 16, 64)
		if err != nil || back != bits {
			t.Errorf("%s: float_bits %q -> %x, want %x", name, got.Columns[0].FloatBits, back, bits)
		}
		if want := strconv.FormatFloat(math.Float64frombits(bits), 'g', -1, 64); got.Columns[0].Value != want {
			t.Errorf("%s: value %q, want %q", name, got.Columns[0].Value, want)
		}
	}
}
