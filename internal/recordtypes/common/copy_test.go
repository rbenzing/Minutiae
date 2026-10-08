package common_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

func TestCopyMapIsDeep(t *testing.T) {
	in := map[string]any{
		"s": "x", "n": int64(3), "f": 1.5, "b": true, "nil": nil,
		"m": map[string]any{"a": []any{"x", map[string]any{"k": int64(1)}}},
		"l": []any{int64(1), []any{"y"}},
	}
	out := common.CopyMap(in)
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("CopyMap = %v, want %v", out, in)
	}
	// nothing is shared with the input
	out["m"].(map[string]any)["a"].([]any)[1].(map[string]any)["k"] = int64(99)
	out["l"].([]any)[0] = "changed"
	out["m"].(map[string]any)["new"] = true
	if in["m"].(map[string]any)["a"].([]any)[1].(map[string]any)["k"] != int64(1) || in["l"].([]any)[0] != int64(1) {
		t.Error("CopyMap aliases the input")
	}
	if _, ok := in["m"].(map[string]any)["new"]; ok {
		t.Error("CopyMap aliases the input map")
	}
	// an empty map stays an empty, non-nil map; a nil map copies to an empty one
	if got := common.CopyMap(map[string]any{}); got == nil || len(got) != 0 {
		t.Errorf("CopyMap(empty) = %#v", got)
	}
	if got := common.CopyMap(nil); got == nil || len(got) != 0 {
		t.Errorf("CopyMap(nil) = %#v", got)
	}
}

// TestCopyMapStopsAtTheDepthTheValidatorRefuses: a hostile 5000-deep value is cut,
// not recursed to the bottom.
func TestCopyMapStopsAtTheDepthTheValidatorRefuses(t *testing.T) {
	deep := map[string]any{}
	cur := deep
	for range 5000 {
		next := map[string]any{}
		cur["k"] = next
		cur = next
	}
	out := common.CopyMap(deep)
	depth := 0
	for v := any(out); ; depth++ {
		m, ok := v.(map[string]any)
		if !ok {
			break
		}
		v = m["k"]
	}
	if depth > common.MaxContainerDepth+2 {
		t.Errorf("CopyMap kept %d levels", depth)
	}
}

func TestNormMapKeepsNumbersExact(t *testing.T) {
	dec := func(s string) map[string]any {
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		var m map[string]any
		if err := d.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	got := common.NormMap(dec(`{"i":3,"neg":-7,"big":9223372036854775807,"f":1.50,"e":1e3,"huge":1e999,"over":9223372036854775808,"max":18446744073709551615,
		"s":"x","b":true,"n":null,"m":{"a":[1,2.5,{"k":4}]}}`))
	want := map[string]any{
		"i": json.Number("3"), "neg": json.Number("-7"), "big": json.Number("9223372036854775807"), "f": json.Number("1.50"),
		"e": json.Number("1e3"), "huge": json.Number("1e999"), "over": json.Number("9223372036854775808"),
		"max": json.Number("18446744073709551615"),
		"s":   "x", "b": true, "n": nil,
		"m": map[string]any{"a": []any{json.Number("1"), json.Number("2.5"), map[string]any{"k": json.Number("4")}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NormMap =\n %#v\nwant\n %#v", got, want)
	}
	// a map without numbers comes back equal and is a copy
	in := map[string]any{"a": []any{"x"}}
	out := common.NormMap(in)
	out["a"].([]any)[0] = "y"
	if in["a"].([]any)[0] != "x" {
		t.Error("NormMap aliases the input")
	}
	if got := common.NormMap(nil); got == nil || len(got) != 0 {
		t.Errorf("NormMap(nil) = %#v", got)
	}
}

func TestNormMapStopsAtDepth(t *testing.T) {
	deep := map[string]any{}
	cur := deep
	for range 5000 {
		next := map[string]any{}
		cur["k"] = next
		cur = next
	}
	out := common.NormMap(deep)
	depth := 0
	for v := any(out); ; depth++ {
		m, ok := v.(map[string]any)
		if !ok {
			break
		}
		v = m["k"]
	}
	if depth > common.MaxContainerDepth+2 {
		t.Errorf("NormMap kept %d levels", depth)
	}
}

func TestIsLive(t *testing.T) {
	obj := func() map[string]any { return map[string]any{"source": "x"} }
	for name, tc := range map[string]struct {
		recovery, snapshot, deleted map[string]any
		want                        bool
	}{
		"nothing":                 {nil, nil, nil, true},
		"recovery":                {obj(), nil, nil, false},
		"snapshot":                {nil, obj(), nil, false},
		"deleted":                 {nil, nil, obj(), false},
		"empty recovery object":   {map[string]any{}, nil, nil, false},
		"empty snapshot object":   {nil, map[string]any{}, nil, false},
		"empty deleted object":    {nil, nil, map[string]any{}, false},
		"all three":               {obj(), obj(), obj(), false},
		"recovery and a snapshot": {obj(), obj(), nil, false},
	} {
		if got := common.IsLive(tc.recovery, tc.snapshot, tc.deleted); got != tc.want {
			t.Errorf("%s: IsLive = %v, want %v", name, got, tc.want)
		}
	}
}
