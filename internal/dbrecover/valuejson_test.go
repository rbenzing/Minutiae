package dbrecover

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func u16(s string, be bool) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		if be {
			out = append(out, byte(u>>8), byte(u))
		} else {
			out = append(out, byte(u), byte(u>>8))
		}
	}
	return out
}

func textV(b []byte, enc sqlitefile.Encoding) sqlitefile.Value {
	return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: b, Len: int64(len(b)), Enc: enc, Serial: uint64(13 + 2*len(b))}
}

func big() *valueBudget { return newValueBudget(1 << 30) }

func TestEncodeValueTable(t *testing.T) {
	lone := []byte{0x3d, 0xd8} // a lone high surrogate, UTF-16LE
	clipped := textV([]byte("ab"), sqlitefile.EncUTF8)
	clipped.Len, clipped.Clipped, clipped.Serial = 10, true, 0
	cases := []struct {
		name string
		v    sqlitefile.Value
		want map[string]any
	}{
		{"null", sqlitefile.Value{Kind: sqlitefile.KindNull}, map[string]any{"i": int64(3), "type": "null"}},
		{"int", sqlitefile.Value{Kind: sqlitefile.KindInt, Int: 42, Serial: 1}, map[string]any{"i": int64(3), "type": "integer", "value": int64(42), "serial": int64(1)}},
		{"int min", sqlitefile.Value{Kind: sqlitefile.KindInt, Int: math.MinInt64}, map[string]any{"i": int64(3), "type": "integer", "value": int64(math.MinInt64)}},
		{"int max", sqlitefile.Value{Kind: sqlitefile.KindInt, Int: math.MaxInt64}, map[string]any{"i": int64(3), "type": "integer", "value": int64(math.MaxInt64)}},
		{"real 1.5", sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: 1.5}, map[string]any{"i": int64(3), "type": "real", "value": "1.5", "float_bits": "3ff8000000000000"}},
		{"real -0", sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: math.Copysign(0, -1)}, map[string]any{"i": int64(3), "type": "real", "value": "-0", "float_bits": "8000000000000000"}},
		{"real subnormal", sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: math.Float64frombits(1)}, map[string]any{"i": int64(3), "type": "real", "value": "5e-324", "float_bits": "0000000000000001"}},
		{"real +Inf", sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: math.Inf(1)}, map[string]any{"i": int64(3), "type": "real", "float_bits": "7ff0000000000000"}},
		{"real -Inf", sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: math.Inf(-1)}, map[string]any{"i": int64(3), "type": "real", "float_bits": "fff0000000000000"}},
		{"real NaN", sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: math.Float64frombits(0x7ff8_0000_dead_beef)}, map[string]any{"i": int64(3), "type": "real", "float_bits": "7ff80000deadbeef"}},
		{"text ascii", textV([]byte("hello"), sqlitefile.EncUTF8), map[string]any{"i": int64(3), "type": "text", "value": "hello", "enc": "UTF-8", "len": int64(5), "serial": int64(23)}},
		{"text multibyte", textV([]byte("h\u00e9\u4e16"), sqlitefile.EncUTF8), map[string]any{"i": int64(3), "type": "text", "value": "h\u00e9\u4e16", "enc": "UTF-8", "len": int64(6), "serial": int64(25)}},
		{"text utf16le", textV(u16("a\u00e9", false), sqlitefile.EncUTF16LE), map[string]any{"i": int64(3), "type": "text", "value": "a\u00e9", "enc": "UTF-16le", "len": int64(4), "serial": int64(21)}},
		{"text utf16be", textV(u16("a\u00e9", true), sqlitefile.EncUTF16BE), map[string]any{"i": int64(3), "type": "text", "value": "a\u00e9", "enc": "UTF-16be", "len": int64(4), "serial": int64(21)}},
		{"text empty", textV([]byte{}, sqlitefile.EncUTF8), map[string]any{"i": int64(3), "type": "text", "value": "", "enc": "UTF-8", "len": int64(0), "serial": int64(13)}},
		{"text lone surrogate", textV(lone, sqlitefile.EncUTF16LE), map[string]any{"i": int64(3), "type": "text", "value_b64": base64.StdEncoding.EncodeToString(lone), "text_valid": false, "enc": "UTF-16le", "len": int64(2), "serial": int64(17)}},
		{"text invalid utf8", textV([]byte{0xff, 'a'}, sqlitefile.EncUTF8), map[string]any{"i": int64(3), "type": "text", "value_b64": "/2E=", "text_valid": false, "enc": "UTF-8", "len": int64(2), "serial": int64(17)}},
		{"text NUL", textV([]byte("a\x00b"), sqlitefile.EncUTF8), map[string]any{"i": int64(3), "type": "text", "value_b64": "YQBi", "text_valid": false, "enc": "UTF-8", "len": int64(3), "serial": int64(19)}},
		{"blob", sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: []byte{0, 1, 2}, Len: 3, Serial: 18}, map[string]any{"i": int64(3), "type": "blob", "value_b64": "AAEC", "len": int64(3), "serial": int64(18)}},
		{"blob empty", sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: []byte{}, Serial: 12}, map[string]any{"i": int64(3), "type": "blob", "value_b64": "", "len": int64(0), "serial": int64(12)}},
		{"omitted text", sqlitefile.Value{Kind: sqlitefile.KindText, Omitted: true, Len: 9000, Enc: sqlitefile.EncUTF8}, map[string]any{"i": int64(3), "type": "text", "omitted": true, "len": int64(9000), "enc": "UTF-8"}},
		{"omitted default expression", sqlitefile.Value{Omitted: true, DefaultUndecided: sqlitefile.DefaultReasonExpression}, map[string]any{"i": int64(3), "type": "null", "omitted": true, "default_undecided": "default is an expression"}},
		{"omitted default inexact", sqlitefile.Value{Omitted: true, DefaultUndecided: sqlitefile.DefaultReasonInexactLiteral}, map[string]any{"i": int64(3), "type": "null", "omitted": true, "default_undecided": "default literal is not converted exactly"}},
		{"unread int", sqlitefile.Value{Kind: sqlitefile.KindInt, Unread: true}, map[string]any{"i": int64(3), "type": "integer", "unread": true}},
		{"unread real", sqlitefile.Value{Kind: sqlitefile.KindFloat, Unread: true, Float: 2.5}, map[string]any{"i": int64(3), "type": "real", "unread": true}},
		{"unread blob", sqlitefile.Value{Kind: sqlitefile.KindBlob, Unread: true, Omitted: true, Len: 10}, map[string]any{"i": int64(3), "type": "blob", "unread": true, "omitted": true, "len": int64(10)}},
		{"clipped blob", sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: []byte{1, 2}, Len: 10, Clipped: true}, map[string]any{"i": int64(3), "type": "blob", "value_b64": "AQI=", "len": int64(10), "clipped": true}},
		{"clipped text", clipped, map[string]any{"i": int64(3), "type": "text", "value": "ab", "enc": "UTF-8", "len": int64(10), "clipped": true}},
	}
	for _, c := range cases {
		got := encodeValue(3, c.v, big())
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %#v\n want %#v", c.name, got, c.want)
		}
		p := validPayload()
		p["columns"] = []any{got}
		if err := validateDBRow(p); err != nil {
			t.Errorf("%s: the validator refuses the encoding: %v", c.name, err)
		}
	}
}

func TestUnreadScalarNeverGetsAValue(t *testing.T) {
	for _, k := range []sqlitefile.Kind{sqlitefile.KindInt, sqlitefile.KindFloat, sqlitefile.KindText, sqlitefile.KindBlob} {
		m := encodeValue(0, sqlitefile.Value{Kind: k, Unread: true, Int: 7, Float: 7, Bytes: []byte("x"), Len: 1}, big())
		for _, key := range []string{"value", "value_b64", "float_bits"} {
			if _, has := m[key]; has {
				t.Errorf("kind %d: an unread value carries %q", k, key)
			}
		}
		if m["unread"] != true {
			t.Errorf("kind %d: unread flag missing: %v", k, m)
		}
	}
}

func TestEncodedBudgetCountsEscapingAndBase64(t *testing.T) {
	ctrl := bytes.Repeat([]byte{0x01}, 8<<20)
	m := map[string]any{"value": string(ctrl)}
	if sz := mustSize(m); sz < 5*int64(len(ctrl)) || sz > 7*int64(len(ctrl)) {
		t.Errorf("8 MiB of control characters costs %d, want about 6x raw", sz)
	}
	if sz := mustSize(map[string]any{"value": strings.Repeat(`"`, 1000)}); sz < 2000 {
		t.Errorf("1000 quotes cost %d, want at least 2000", sz)
	}
	blob := bytes.Repeat([]byte{0xab}, 1<<20)
	if sz := mustSize(map[string]any{"value_b64": base64.StdEncoding.EncodeToString(blob)}); sz < 1_398_000 || sz > 1_399_000 {
		t.Errorf("1 MiB blob costs %d, want about 1.33x", sz)
	}
	// through encodeValue: the charge is the encoded size and the row stays under the payload cap
	const payloadCap = 16 << 20
	b := newValueBudget(payloadCap)
	var total int64
	var cols []any
	for i, v := range []sqlitefile.Value{
		textV(ctrl[:1<<20], sqlitefile.EncUTF8),
		{Kind: sqlitefile.KindBlob, Bytes: blob, Len: int64(len(blob))},
		textV(ctrl, sqlitefile.EncUTF8), // about 48 MiB encoded: must degrade
	} {
		before := b.left
		e := encodeValue(i, v, b)
		cols = append(cols, e)
		total += before - b.left
		if want := max(mustSize(e)-mustSize(degraded(i, v)), 0); before-b.left != want {
			t.Errorf("column %d: charged %d, want the extra %d over the reserved degraded form", i, before-b.left, want)
		}
	}
	if cols[2].(map[string]any)["omitted"] != "payload-budget" {
		t.Error("the 8 MiB control-character text was not degraded")
	}
	if cols[0].(map[string]any)["value"] == nil || cols[1].(map[string]any)["value_b64"] == nil {
		t.Error("columns that fit were degraded")
	}
	if total > payloadCap || b.left < 0 {
		t.Errorf("charged %d of %d (left %d)", total, payloadCap, b.left)
	}
}

func TestValueBudgetDegradesLoudly(t *testing.T) {
	b := newValueBudget(100)
	small := encodeValue(0, sqlitefile.Value{Kind: sqlitefile.KindInt, Int: 1}, b)
	if small["value"] != int64(1) {
		t.Fatalf("a small value degraded: %v", small)
	}
	left := b.left
	huge := encodeValue(1, textV(bytes.Repeat([]byte("a"), 5000), sqlitefile.EncUTF8), b)
	want := map[string]any{"i": int64(1), "type": "text", "len": int64(5000), "omitted": "payload-budget"}
	if !reflect.DeepEqual(huge, want) {
		t.Fatalf("degraded = %#v, want %#v", huge, want)
	}
	if b.left > left || b.left < 0 {
		t.Errorf("budget %d after degrading, was %d", b.left, left)
	}
	b = newValueBudget(0)
	m := encodeValue(2, sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: bytes.Repeat([]byte("a"), 100), Len: 100}, b)
	if m["omitted"] != "payload-budget" || m["value_b64"] != nil || b.left != 0 {
		t.Errorf("exhausted budget: %v (left %d)", m, b.left)
	}
	p := validPayload()
	p["columns"] = []any{huge}
	if err := validateDBRow(p); err != nil {
		t.Errorf("the validator refuses a degraded column: %v", err)
	}
}

// TestEncodedMapsAreWriterAcceptable runs every kind of column, and an
// unrepresentable table name, through a real Writer.Add.
func TestEncodedMapsAreWriterAcceptable(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", []byte("x"))
	vals := []sqlitefile.Value{
		{Kind: sqlitefile.KindNull},
		{Kind: sqlitefile.KindInt, Int: math.MinInt64},
		{Kind: sqlitefile.KindFloat, Float: 1.5},
		{Kind: sqlitefile.KindFloat, Float: math.Float64frombits(0x7ff8_0000_dead_beef)},
		{Kind: sqlitefile.KindFloat, Float: math.Inf(-1)},
		textV([]byte("plain"), sqlitefile.EncUTF8),
		textV([]byte("a\x00b"), sqlitefile.EncUTF8),
		textV([]byte{0xff}, sqlitefile.EncUTF8),
		textV(u16("x\U0001F600", true), sqlitefile.EncUTF16BE),
		{Kind: sqlitefile.KindBlob, Bytes: []byte{0, 1, 2}, Len: 3},
		{Kind: sqlitefile.KindBlob, Omitted: true, Len: 1 << 20},
		{Kind: sqlitefile.KindText, Unread: true},
		{Kind: sqlitefile.KindBlob, Bytes: []byte{9}, Len: 5, Clipped: true},
	}
	b := big()
	cols := make([]any, len(vals))
	for i, v := range vals {
		cols[i] = encodeValue(i, v, b)
	}
	p := validPayload()
	delete(p, "table")
	p["columns"] = cols
	encodeName("table", "t\x00bad", p)
	res := recordstest.Ingest(t, c, records.Parser{Name: "t", Version: "1"}, []string{art.ID}, []records.Record{{
		Type: TypeName, ArtifactID: art.ID, Summary: "x", Deleted: true, Recovery: "sqlite-freelist", Payload: p,
	}})
	if res.Records != 1 {
		t.Fatalf("stored %d records, want 1", res.Records)
	}
	rd, err := records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	full, err := rd.Get(context.Background(), res.FirstID)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Columns []map[string]any `json:"columns"`
		Table   string           `json:"table_b64"`
		Valid   *bool            `json:"table_valid"`
	}
	if err := json.Unmarshal(full.Payload, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Columns) != len(vals) || back.Valid == nil || *back.Valid || back.Table != base64.StdEncoding.EncodeToString([]byte("t\x00bad")) {
		t.Errorf("round trip: %d columns, table_b64 %q valid %v", len(back.Columns), back.Table, back.Valid)
	}
	if back.Columns[3]["float_bits"] != "7ff80000deadbeef" {
		t.Errorf("NaN bits = %v", back.Columns[3]["float_bits"])
	}
}

func TestEncodeNameIsExact(t *testing.T) {
	for name, s := range map[string]string{
		"nul":      "a\x00b",
		"invalid":  "a\xff\xfeb",
		"1 MiB":    strings.Repeat("n", 1<<20),
		"long nul": strings.Repeat("n", 1<<20) + "\x00",
		"plain":    "messages",
		"bidi":     "a\u202eb",
		"empty":    "",
	} {
		out := map[string]any{}
		encodeName("table", s, out)
		var back string
		_, plain := out["table"]
		_, wrapped := out["table_b64"]
		if plain == wrapped {
			t.Fatalf("%s: plain and base64 forms: %v", name, plain)
		}
		if plain {
			back = out["table"].(string)
			if !utf8.ValidString(back) || strings.ContainsRune(back, 0) || len(back) > maxNameBytes {
				t.Errorf("%s: the plain form holds an unrepresentable name", name)
			}
		} else {
			raw, err := base64.StdEncoding.DecodeString(out["table_b64"].(string))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			back = string(raw)
		}
		if back != s {
			t.Errorf("%s: the encoding does not round-trip", name)
		}
		invalid := !utf8.ValidString(s) || strings.ContainsRune(s, 0)
		valid, has := out["table_valid"]
		if invalid && (!has || valid != false) {
			t.Errorf("%s: table_valid = %v, want false", name, valid)
		}
		if !invalid && has {
			t.Errorf("%s: table_valid set for a valid name", name)
		}
		p := validPayload()
		delete(p, "table")
		for k, v := range out {
			p[k] = v
		}
		if err := validateDBRow(p); err != nil {
			t.Errorf("%s: the validator refuses the encoding: %v", name, err)
		}
	}
}

func TestUndecidedDefaultReasonIsKeptInOutput(t *testing.T) {
	for _, r := range []sqlitefile.DefaultReason{sqlitefile.DefaultReasonExpression, sqlitefile.DefaultReasonInexactLiteral} {
		v := sqlitefile.Value{Omitted: true, DefaultUndecided: r}
		if got, want := summaryValue(v), "<omitted: "+r.String()+">"; got != want {
			t.Errorf("summary %q, want %q", got, want)
		}
		if got := summaryValue(sqlitefile.Value{Omitted: true}); got != "<omitted>" {
			t.Errorf("plain omitted summary %q", got)
		}
		p := validPayload()
		p["columns"] = []any{map[string]any{"i": int64(0), "type": "null", "omitted": true, "default_undecided": r.String()}}
		if err := validateDBRow(p); err != nil {
			t.Errorf("reason %q refused: %v", r, err)
		}
	}
	p := validPayload()
	p["columns"] = []any{map[string]any{"i": int64(0), "type": "null", "omitted": true, "default_undecided": "because"}}
	if validateDBRow(p) == nil {
		t.Error("an unknown reason was accepted")
	}
}

// U32: the validator's reason list is exactly the DefaultReason enum.
func TestDefaultReasonsListEqualsTheEnum(t *testing.T) {
	var enum []string
	for r := sqlitefile.DefaultReasonNone + 1; r.String() != "default?"; r++ {
		enum = append(enum, r.String())
	}
	if len(enum) < 2 {
		t.Fatalf("parsed %d reasons from the enum", len(enum))
	}
	if len(defaultReasons) != len(enum) {
		t.Fatalf("defaultReasons has %d entries, the enum %d: %v vs %v", len(defaultReasons), len(enum), defaultReasons, enum)
	}
	for _, s := range enum {
		if !slices.Contains(defaultReasons, s) {
			t.Errorf("defaultReasons lacks %q", s)
		}
	}
}

// U33: the payload-budget form of an undecided default keeps its typed reason.
func TestDegradedFormKeepsTheUndecidedDefaultReason(t *testing.T) {
	for _, r := range []sqlitefile.DefaultReason{sqlitefile.DefaultReasonExpression, sqlitefile.DefaultReasonInexactLiteral} {
		m := degraded(0, sqlitefile.Value{Omitted: true, DefaultUndecided: r})
		if m["omitted"] != "payload-budget" || m["default_undecided"] != r.String() {
			t.Errorf("degraded %v lacks the reason %q", m, r)
		}
		p := validPayload()
		p["columns"] = []any{m}
		if err := validateDBRow(p); err != nil {
			t.Errorf("the validator refuses the degraded form: %v", err)
		}
	}
	m := degraded(0, sqlitefile.Value{Omitted: true})
	if _, has := m["default_undecided"]; has {
		t.Errorf("a plain omitted value gained a reason: %v", m)
	}
}

// U34: a reason without an omitted column is not a valid payload.
func TestValidatorRefusesAReasonOnAColumnThatIsNotOmitted(t *testing.T) {
	for _, col := range []map[string]any{
		{"i": int64(0), "type": "null", "default_undecided": "default is an expression"},
		{"i": int64(0), "type": "null", "omitted": false, "default_undecided": "default is an expression"},
		{"i": int64(0), "type": "integer", "value": int64(1), "default_undecided": "default is an expression"},
	} {
		p := validPayload()
		p["columns"] = []any{col}
		if validateDBRow(p) == nil {
			t.Errorf("accepted %v", col)
		}
	}
}
