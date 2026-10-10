package dbrecover

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

func nest(depth int) any {
	v := any("x")
	for range depth {
		v = []any{v}
	}
	return v
}

func strs(n int) []any {
	l := make([]any, n)
	for i := range l {
		l[i] = "a"
	}
	return l
}

func TestValidatorBoundaries(t *testing.T) {
	type tc struct {
		name string
		mut  func(p map[string]any)
		ok   bool
	}
	cases := []tc{
		{"cell MaxInt32", func(p map[string]any) { p["cell"] = int64(math.MaxInt32) }, true},
		{"cell MaxInt32+1", func(p map[string]any) { p["cell"] = int64(math.MaxInt32) + 1 }, false},
		{"cell MaxInt64", func(p map[string]any) { p["cell"] = int64(math.MaxInt64) }, false},
		{"wal depth 8", func(p map[string]any) { p["wal"] = nest(7) }, true}, // 7 lists + string leaf at depth 7
		{"wal depth 8 leaf", func(p map[string]any) { p["wal"] = nest(8) }, true},
		{"wal depth 9", func(p map[string]any) { p["wal"] = nest(9) }, false},
		{"wal depth 64", func(p map[string]any) { p["wal"] = nest(64) }, false},
		{"wal 65535 nodes", func(p map[string]any) { p["wal"] = strs(maxNodes - 1) }, true},
		{"wal 65536 nodes", func(p map[string]any) { p["wal"] = strs(maxNodes) }, false},
		{"wal 70000 nodes", func(p map[string]any) { p["wal"] = strs(70000) }, false},
		{"notes at cap", func(p map[string]any) { p["notes"] = strs(maxNodes) }, true},
		{"notes over cap", func(p map[string]any) { p["notes"] = strs(maxNodes + 1) }, false},
		{"name at cap", func(p map[string]any) { p["table"] = strings.Repeat("a", maxNameBytes) }, true},
		{"name over cap", func(p map[string]any) { p["table"] = strings.Repeat("a", maxNameBytes+1) }, false},
		{"table_valid string", func(p map[string]any) { p["table_valid"] = "x" }, false},
		{"table_valid bool", func(p map[string]any) { p["table_valid"] = false }, true},
		{"sibling_pairing_wal x", func(p map[string]any) { p["sibling_pairing_wal"] = "x" }, false},
		{"sibling_pairing_wal by-name", func(p map[string]any) { p["sibling_pairing_wal"] = "by-name" }, true},
		{"sibling_pairing_journal explicit", func(p map[string]any) { p["sibling_pairing_journal"] = "explicit" }, true},
		{"sibling_pairing_journal none", func(p map[string]any) { p["sibling_pairing_journal"] = "none" }, true},
		{"legacy single sibling_pairing is gone", func(p map[string]any) { p["sibling_pairing"] = "by-name" }, false},
	}
	carved := func(m map[string]any) func(p map[string]any) {
		return func(p map[string]any) { delete(p, "cell"); p["carved"] = m }
	}
	good := func() map[string]any {
		return map[string]any{"span_kind": "gap", "span_offset": int64(1), "span_length": int64(2), "slack_region": "gap"}
	}
	four := good()
	four["extra"] = int64(1)
	numRegion := good()
	numRegion["slack_region"] = int64(3)
	noRegion := map[string]any{"span_kind": "gap", "span_offset": int64(1), "span_length": int64(2)}
	numKind := good()
	numKind["span_kind"] = int64(3)
	longKind := good()
	longKind["span_kind"] = strings.Repeat("k", maxNameBytes+1)
	negOff := good()
	negOff["span_offset"] = int64(-1)
	negLen := good()
	negLen["span_length"] = int64(-1)
	cases = append(cases,
		tc{"carved good", carved(good()), true},
		tc{"carved 5 keys", carved(four), false},
		tc{"carved slack_region number", carved(numRegion), false},
		tc{"carved without slack_region", carved(noRegion), false},
		tc{"carved span_kind number", carved(numKind), false},
		tc{"carved span_kind too long", carved(longKind), false},
		tc{"carved negative offset", carved(negOff), false},
		tc{"carved negative length", carved(negLen), false},
		tc{"cell with carved", func(p map[string]any) { p["carved"] = good() }, false},
	)
	for _, c := range cases {
		p := validPayload()
		c.mut(p)
		err := validateDBRow(p)
		if c.ok && err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
		if !c.ok && (err == nil || !errors.Is(err, records.ErrInvalidPayload)) {
			t.Errorf("%s: err = %v, want ErrInvalidPayload", c.name, err)
		}
	}
}

func TestNilPayloadIsRefusedByName(t *testing.T) {
	err := validateDBRow(nil)
	if err == nil || !strings.Contains(err.Error(), "payload is nil") {
		t.Fatalf("nil payload: %v", err)
	}
}

func TestValidatorPanicIsRecovered(t *testing.T) {
	defer SetCheckForTest(func(map[string]any) error { panic("boom") })()
	err := validateDBRow(validPayload())
	if err == nil || !errors.Is(err, records.ErrInvalidPayload) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("panic not turned into a typed error: %v", err)
	}
}

func TestColumnShapeConsistency(t *testing.T) {
	col := func(m map[string]any) map[string]any {
		p := validPayload()
		p["columns"] = []any{m}
		return p
	}
	for name, m := range map[string]map[string]any{
		"integer with text":  {"i": int64(0), "type": "integer", "value": "abc"},
		"text with number":   {"i": int64(0), "type": "text", "value": int64(1)},
		"text with NUL":      {"i": int64(0), "type": "text", "value": "a\x00b"},
		"index past columns": {"i": int64(1 << 40), "type": "null"},
	} {
		if err := validateDBRow(col(m)); err == nil || !errors.Is(err, records.ErrInvalidPayload) {
			t.Errorf("%s: %v", name, err)
		}
	}
	p := validPayload()
	p["columns"] = []any{map[string]any{"i": int64(1), "type": "null"}, map[string]any{"i": int64(0), "type": "null"}}
	if validateDBRow(p) == nil {
		t.Error("column indexes out of order accepted")
	}
	p["columns"] = []any{map[string]any{"i": int64(0), "type": "integer", "value": json.Number("12")}}
	if err := validateDBRow(p); err != nil {
		t.Errorf("json.Number integer: %v", err)
	}
}
