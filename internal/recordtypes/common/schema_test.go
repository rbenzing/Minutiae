package common_test

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

type obj = map[string]any

func i64(v int64) *int64 { return &v }

// check validates payload against a one-field schema and returns the error text.
func check(t *testing.T, f common.Field, payload obj) string {
	t.Helper()
	err := common.Schema{Fields: []common.Field{f}}.Validate(payload)
	if err == nil {
		return ""
	}
	return err.Error()
}

func wantOK(t *testing.T, f common.Field, v any) {
	t.Helper()
	if msg := check(t, f, obj{"f": v}); msg != "" {
		t.Errorf("%T(%v) refused: %s", v, v, msg)
	}
}

func wantErr(t *testing.T, f common.Field, v any, contains string) {
	t.Helper()
	msg := check(t, f, obj{"f": v})
	if msg == "" {
		t.Errorf("%T(%v) was accepted, want an error containing %q", v, v, contains)
	} else if !strings.Contains(msg, contains) {
		t.Errorf("%T(%v): error %q does not contain %q", v, v, msg, contains)
	}
}

func TestSchemaValidate(t *testing.T) {
	t.Run("required", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KString, Required: true}
		if msg := check(t, f, obj{}); msg != "payload.f: required field is missing" {
			t.Errorf("missing: %q", msg)
		}
		if msg := check(t, f, obj{"f": "x"}); msg != "" {
			t.Errorf("present: %q", msg)
		}
		opt := common.Field{Name: "f", Kind: common.KString}
		if msg := check(t, opt, obj{}); msg != "" {
			t.Errorf("an absent optional field is fine: %q", msg)
		}
		wantErr(t, opt, nil, "must be a string, not null") // present as null is wrong, not absent
	})
	t.Run("wrong type", func(t *testing.T) {
		wantErr(t, common.Field{Name: "f", Kind: common.KString}, 5, "payload.f: must be a string, not a number")
		wantErr(t, common.Field{Name: "f", Kind: common.KBool}, "true", "must be a boolean, not a string")
		wantErr(t, common.Field{Name: "f", Kind: common.KInt}, "5", "must be an integer, not a string")
		wantErr(t, common.Field{Name: "f", Kind: common.KObject}, []any{}, "must be an object, not an array")
		wantErr(t, common.Field{Name: "f", Kind: common.KArray}, obj{}, "must be an array, not an object")
		wantErr(t, common.Field{Name: "f", Kind: common.KRaw}, "x", "must be an object")
		wantErr(t, common.Field{Name: "f", Kind: common.KString}, struct{}{}, "not a value of an unsupported type")
		wantOK(t, common.Field{Name: "f", Kind: common.KBool}, false)
	})
	t.Run("string", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KString, NonEmpty: true, MaxLen: 5}
		wantOK(t, f, "abcde")
		wantErr(t, f, "", "must not be empty")
		wantErr(t, f, "abcdef", "longer than 5 bytes")
		wantOK(t, common.Field{Name: "f", Kind: common.KString}, "")
	})
	t.Run("token", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KToken}
		for _, ok := range []string{"sms", "imessage", "a", "whats_app2", "a" + strings.Repeat("b", 31)} {
			wantOK(t, f, ok)
		}
		for _, bad := range []string{"", "SMS", "1sms", "_sms", "sms-2", "sms ", "a" + strings.Repeat("b", 32), "émoji"} {
			wantErr(t, f, bad, "not a token")
		}
	})
	t.Run("enum list and unknown", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KEnum, Enum: []string{"from", "to", "cc", "bcc", "member"}}
		wantOK(t, f, "from")
		wantOK(t, f, "member")
		wantOK(t, f, "unknown")
		wantErr(t, f, "From", "payload.f: not one of from,to,cc,bcc,member")
		wantErr(t, f, "", "not one of")
		wantErr(t, f, 1, "must be a string")
	})
	t.Run("id string", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KIDString}
		wantOK(t, f, "abc")
		wantOK(t, f, 42)
		wantOK(t, f, int64(-7))
		wantOK(t, f, json.Number("12345678901234"))
		wantErr(t, f, "", "must not be empty")
		wantErr(t, f, strings.Repeat("x", 1025), "longer than 1024 bytes")
		wantOK(t, f, strings.Repeat("x", 1024))
		wantErr(t, f, 1.5, "a string or an integer")
		wantErr(t, f, true, "a string or an integer")
		wantOK(t, common.Field{Name: "f", Kind: common.KIDString, MaxLen: 3}, "abc")
		wantErr(t, common.Field{Name: "f", Kind: common.KIDString, MaxLen: 3}, "abcd", "longer than 3 bytes")
	})
	t.Run("int", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KInt}
		wantOK(t, f, 7)
		wantOK(t, f, int64(math.MinInt64))
		wantOK(t, f, uint64(math.MaxInt64))
		wantErr(t, f, uint64(math.MaxInt64)+1, "must be an integer")
		wantOK(t, f, json.Number("42"))
		wantOK(t, f, json.Number("-42"))
		wantOK(t, f, json.Number("9223372036854775807"))
		wantOK(t, f, json.Number("1.0"))
		wantOK(t, f, json.Number("1e3"))
		wantErr(t, f, json.Number("9223372036854775808"), "must be an integer")
		wantErr(t, f, json.Number("1.5"), "must be an integer")
		wantErr(t, f, json.Number("1e400"), "must be an integer")
		wantErr(t, f, json.Number("0x10"), "must be an integer")
		wantErr(t, f, json.Number("NaN"), "must be an integer")
		wantErr(t, f, json.Number(""), "must be an integer")
		wantErr(t, f, 1.0, "must be an integer") // a float64 is not an integer here
		withMin := common.Field{Name: "f", Kind: common.KInt, Min: i64(0)}
		wantOK(t, withMin, 0)
		wantErr(t, withMin, -1, "below the minimum 0")
		wantErr(t, withMin, json.Number("-1"), "below the minimum 0")
	})
	t.Run("number", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KNumber, Min: i64(0)}
		wantOK(t, f, 0)
		wantOK(t, f, 1.5)
		wantOK(t, f, int64(3))
		wantOK(t, f, uint64(math.MaxUint64))
		wantOK(t, f, json.Number("2.50"))
		wantOK(t, f, json.Number("1e3"))
		wantErr(t, f, -0.5, "below the minimum 0")
		wantErr(t, f, json.Number("-1e-3"), "below the minimum 0")
		wantErr(t, f, math.NaN(), "must be a finite number")
		wantErr(t, f, math.Inf(1), "must be a finite number")
		wantErr(t, f, math.Inf(-1), "must be a finite number")
		for _, bad := range []string{"NaN", "Inf", "+Inf", "-inf", "1e999", "0x1p-2", "1_0", ".5", "5.", "01", "+1", "1e", "--1", " 1", "1 ", ""} {
			wantErr(t, f, json.Number(bad), "must be a finite number")
		}
		wantErr(t, f, "1.5", "must be a finite number")
	})
	t.Run("hex64 case", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KHex64}
		wantOK(t, f, strings.Repeat("0123456789abcdef", 4))
		wantErr(t, f, strings.Repeat("0123456789ABCDEF", 4), "not 64 lowercase hex digits")
		wantErr(t, f, strings.Repeat("a", 63), "not 64 lowercase hex digits")
		wantErr(t, f, strings.Repeat("a", 65), "not 64 lowercase hex digits")
		wantErr(t, f, strings.Repeat("g", 64), "not 64 lowercase hex digits")
		wantErr(t, f, 5, "must be a string")
	})
	t.Run("date forms", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KDate}
		for _, ok := range []string{"2024-02-29", "1999-12-31", "0001-01-01", "--02-29", "--12-31", "--01-01", "2000-02-29", "1900-01-31"} {
			wantOK(t, f, ok)
		}
		for _, bad := range []string{"2023-02-29", "1900-02-29", "2024-13-01", "2024-00-10", "2024-04-31", "2024-01-00", "--13-01", "--04-31", "--02-30",
			"2024-1-01", "24-01-01", "2024/01/01", "2024-01-011", "-02-29", "---02-29", "", "20240101", "2024-01-0a", "--0x-01", "\u0662\u0660\u0662\u0664-\u0660\u0661-\u0660\u0661"} {
			wantErr(t, f, bad, "not a date")
		}
	})
	t.Run("object", func(t *testing.T) {
		sub := &common.Schema{Fields: []common.Field{{Name: "id", Kind: common.KInt, Required: true}}}
		f := common.Field{Name: "f", Kind: common.KObject, Obj: sub}
		wantOK(t, f, obj{"id": 1, "extra": "fine"})
		wantErr(t, f, obj{}, "payload.f.id: required field is missing")
		wantErr(t, f, obj{"id": "x"}, "payload.f.id: must be an integer")
		wantOK(t, common.Field{Name: "f", Kind: common.KObject}, obj{"anything": []any{1}})
	})
	t.Run("array of objects", func(t *testing.T) {
		role := common.Field{Name: "role", Kind: common.KEnum, Enum: []string{"from", "to", "cc", "bcc", "member"}, Required: true}
		f := common.Field{Name: "participants", Kind: common.KArray, Obj: &common.Schema{Fields: []common.Field{role}}}
		good := []any{obj{"role": "from"}, obj{"role": "to"}, obj{"role": "member", "x": 1}}
		if msg := check(t, f, obj{"participants": good}); msg != "" {
			t.Errorf("good array: %s", msg)
		}
		bad := []any{obj{"role": "from"}, obj{"role": "to"}, obj{"role": "cc"}, obj{"role": "owner"}}
		want := "payload.participants[3].role: not one of from,to,cc,bcc,member"
		if msg := check(t, f, obj{"participants": bad}); msg != want {
			t.Errorf("error = %q, want %q", msg, want)
		}
		if msg := check(t, f, obj{"participants": []any{"x"}}); msg != "payload.participants[0]: must be an object, not a string" {
			t.Errorf("non-object element: %q", msg)
		}
		if msg := check(t, f, obj{"participants": []any{obj{}}}); msg != "payload.participants[0].role: required field is missing" {
			t.Errorf("missing role: %q", msg)
		}
	})
	t.Run("string array", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KStringArray}
		wantOK(t, f, []any{})
		wantOK(t, f, []any{"a", "b"})
		wantErr(t, f, []any{"a", 1}, "payload.f[1]: must be a string, not a number")
		wantErr(t, f, []string{"a"}, "must be an array")
	})
	t.Run("array cap 10,000 and 10,001", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KStringArray}
		arr := func(n int) []any {
			a := make([]any, n)
			for i := range a {
				a[i] = "x"
			}
			return a
		}
		wantOK(t, f, arr(10000))
		wantErr(t, f, arr(10001), "more than 10000 entries")
		wantOK(t, common.Field{Name: "f", Kind: common.KArray}, arr(10000))
		wantErr(t, common.Field{Name: "f", Kind: common.KArray}, arr(10001), "more than 10000 entries")
		small := common.Field{Name: "f", Kind: common.KArray, MaxLen: 2}
		wantOK(t, small, arr(2))
		wantErr(t, small, arr(3), "more than 2 entries")
	})
	t.Run("raw holds scalars and nested values", func(t *testing.T) {
		f := common.Field{Name: "f", Kind: common.KRaw}
		wantOK(t, f, obj{"a": 1, "b": "x", "c": nil, "d": true, "e": 1.5, "f": json.Number("7"), "g": uint64(math.MaxUint64), "h": int64(2)})
		wantOK(t, f, obj{"a": []any{1, "x", []any{}}})
		wantErr(t, f, obj{"a": math.NaN()}, "payload.f: holds a number that is not finite")
		wantErr(t, f, obj{"a": json.Number("NaN")}, "not a finite JSON number")
		wantErr(t, f, obj{"a": struct{}{}}, "unsupported type")
		wantErr(t, f, obj{"a": []string{"x"}}, "unsupported type")
		wantErr(t, f, obj{"a": make([]any, 10001)}, "more than 10000 entries")
	})
	t.Run("depth 4 and 5", func(t *testing.T) {
		raw := common.Field{Name: "f", Kind: common.KRaw}
		// payload (1) > f (2) > a (3) > b (4): allowed; one more level (5) is not
		wantOK(t, raw, obj{"a": obj{"b": "x"}})
		wantOK(t, raw, obj{"a": obj{"b": obj{}}}) // an empty container at level 4 is fine: it is level 4
		wantErr(t, raw, obj{"a": obj{"b": obj{"c": obj{}}}}, "nested deeper than 4 levels")
		wantErr(t, raw, obj{"a": []any{[]any{[]any{}}}}, "nested deeper than 4 levels")
		wantOK(t, raw, obj{"a": []any{[]any{}}})
		// the same through schema objects: payload (1) > o1 (2) > o2 (3) > o3 (4) > o4 (5)
		leaf := &common.Schema{}
		l3 := &common.Schema{Fields: []common.Field{{Name: "o4", Kind: common.KObject, Obj: leaf}}}
		l2 := &common.Schema{Fields: []common.Field{{Name: "o3", Kind: common.KObject, Obj: l3}}}
		l1 := &common.Schema{Fields: []common.Field{{Name: "o2", Kind: common.KObject, Obj: l2}}}
		f := common.Field{Name: "o1", Kind: common.KObject, Obj: l1}
		if msg := check(t, f, obj{"o1": obj{"o2": obj{"o3": obj{}}}}); msg != "" {
			t.Errorf("depth 4: %s", msg)
		}
		if msg := check(t, f, obj{"o1": obj{"o2": obj{"o3": obj{"o4": obj{}}}}}); !strings.Contains(msg, "nested deeper than 4 levels") {
			t.Errorf("depth 5: %q", msg)
		}
		// an array of objects with an array inside: payload (1) > array (2) > element (3) > inner array (4) > its elements (5)
		inner := &common.Schema{Fields: []common.Field{{Name: "xs", Kind: common.KStringArray}}}
		arr := common.Field{Name: "a", Kind: common.KArray, Obj: inner}
		if msg := check(t, arr, obj{"a": []any{obj{"xs": []any{"s"}}}}); msg != "" {
			t.Errorf("array in an element: %s", msg)
		}
		deep := &common.Schema{Fields: []common.Field{{Name: "ys", Kind: common.KArray, Obj: inner}}}
		arr2 := common.Field{Name: "a", Kind: common.KArray, Obj: deep}
		if msg := check(t, arr2, obj{"a": []any{obj{"ys": []any{obj{}}}}}); !strings.Contains(msg, "nested deeper than 4 levels") {
			t.Errorf("elements at level 5: %q", msg)
		}
	})
	t.Run("at least one with an empty string", func(t *testing.T) {
		s := common.Schema{AtLeastOne: []string{"display_name", "phones"}}
		if err := s.Validate(obj{"display_name": "Ann"}); err != nil {
			t.Errorf("one present: %v", err)
		}
		if err := s.Validate(obj{"phones": []any{obj{}}}); err != nil {
			t.Errorf("a non-empty array counts: %v", err)
		}
		for name, p := range map[string]obj{
			"none":         {},
			"empty string": {"display_name": ""},
			"empty array":  {"phones": []any{}},
			"empty object": {"display_name": obj{}},
			"null":         {"display_name": nil},
			"both empty":   {"display_name": "", "phones": []any{}},
		} {
			err := s.Validate(p)
			if err == nil || !strings.Contains(err.Error(), "at least one of display_name,phones") {
				t.Errorf("%s: %v", name, err)
			}
		}
		if err := s.Validate(obj{"display_name": true}); err != nil {
			t.Errorf("a non-empty scalar counts: %v", err)
		}
	})
	t.Run("cross error is wrapped", func(t *testing.T) {
		sentinel := errors.New("participants is empty without participants_unknown")
		s := common.Schema{Cross: func(m map[string]any) error { return sentinel }}
		err := s.Validate(obj{})
		if !errors.Is(err, sentinel) || !strings.HasPrefix(err.Error(), "payload: ") {
			t.Errorf("Cross error = %v", err)
		}
		// it runs after the field checks
		ran := false
		s2 := common.Schema{Fields: []common.Field{{Name: "a", Kind: common.KBool, Required: true}}, Cross: func(map[string]any) error { ran = true; return nil }}
		if err := s2.Validate(obj{}); err == nil || ran {
			t.Errorf("Cross ran after a field error (ran=%v, err=%v)", ran, err)
		}
		if err := s2.Validate(obj{"a": true}); err != nil || !ran {
			t.Errorf("Cross did not run (ran=%v, err=%v)", ran, err)
		}
		// a panicking Cross is an error, not a crash
		s3 := common.Schema{Cross: func(map[string]any) error { panic("boom") }}
		if err := s3.Validate(obj{}); err == nil || strings.Contains(err.Error(), "boom") {
			t.Errorf("panicking Cross: %v", err)
		}
	})
	t.Run("unknown fields are allowed", func(t *testing.T) {
		s := common.Schema{Fields: []common.Field{{Name: "a", Kind: common.KBool}}}
		if err := s.Validate(obj{"a": true, "unexpected": []any{1, obj{"x": nil}}, "z": struct{}{}}); err != nil {
			t.Errorf("unknown fields refused: %v", err)
		}
	})
	t.Run("nil map", func(t *testing.T) {
		if err := (common.Schema{}).Validate(nil); err != nil {
			t.Errorf("nil payload, no rules: %v", err)
		}
		s := common.Schema{Fields: []common.Field{{Name: "a", Kind: common.KBool, Required: true}}}
		if err := s.Validate(nil); err == nil || err.Error() != "payload.a: required field is missing" {
			t.Errorf("nil payload with a required field: %v", err)
		}
		if err := (common.Schema{AtLeastOne: []string{"a"}}).Validate(nil); err == nil {
			t.Error("nil payload satisfied AtLeastOne")
		}
	})
	t.Run("first error in field order", func(t *testing.T) {
		s := common.Schema{Fields: []common.Field{
			{Name: "b", Kind: common.KBool, Required: true},
			{Name: "a", Kind: common.KBool, Required: true},
		}}
		for range 20 {
			if err := s.Validate(obj{}); err == nil || err.Error() != "payload.b: required field is missing" {
				t.Fatalf("error = %v", err)
			}
		}
	})
	t.Run("the error type names the path", func(t *testing.T) {
		err := common.Schema{Fields: []common.Field{{Name: "a", Kind: common.KBool}}}.Validate(obj{"a": 1})
		var se *common.SchemaError
		if !errors.As(err, &se) || se.Path != "payload.a" {
			t.Errorf("error = %#v", err)
		}
	})
	t.Run("work is bounded", func(t *testing.T) {
		// 4 levels of 10,000-entry arrays would be 10^12 values
		inner := make([]any, 10000)
		for i := range inner {
			inner[i] = []any{}
		}
		outer := make([]any, 10000)
		for i := range outer {
			outer[i] = inner
		}
		err := common.Schema{Fields: []common.Field{{Name: "raw", Kind: common.KRaw}}}.Validate(obj{"raw": obj{"a": outer}})
		if err == nil {
			t.Error("a payload of 10^8 values was accepted")
		}
	})
	t.Run("a field with no kind is an error, not a panic", func(t *testing.T) {
		wantErr(t, common.Field{Name: "f"}, 1, "no known kind")
	})
}

func TestCommonFields(t *testing.T) {
	schema := common.Schema{Fields: common.CommonFields()}
	ok := func(name string, p obj) {
		t.Helper()
		if err := schema.Validate(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := func(name string, p obj, contains string) {
		t.Helper()
		err := schema.Validate(p)
		if err == nil || !strings.Contains(err.Error(), contains) {
			t.Errorf("%s: error %v, want one containing %q", name, err, contains)
		}
	}
	ok("empty", obj{})
	ok("raw", obj{"raw": obj{"type": 1, "date": json.Number("1700000000000"), "nested": obj{"k": []any{"a"}}}})
	bad("raw not an object", obj{"raw": "x"}, "payload.raw: must be an object")
	for _, rel := range []string{"absent-from-live", "superseded-version", "uncommitted", "from-recovered-artifact", "unknown"} {
		ok("relation "+rel, obj{"recovery": obj{"relation": rel}})
	}
	bad("relation", obj{"recovery": obj{"relation": "found"}}, "payload.recovery.relation: not one of absent-from-live,superseded-version,uncommitted,from-recovered-artifact")
	ok("recovery with free keys", obj{"recovery": obj{"relation": "uncommitted", "via": "wal", "table_basis": "x",
		"wal": obj{"frame": 3, "salt1": json.Number("12"), "committed": false}, "notes": []any{"a", "b"}}})
	bad("recovery key too deep", obj{"recovery": obj{"wal": obj{"a": obj{"b": obj{}}}}}, "payload.recovery: another key holds an unsupported value or nests too deep")
	bad("recovery key bad value", obj{"recovery": obj{"via": math.NaN()}}, "another key holds")
	bad("recovery not an object", obj{"recovery": []any{}}, "payload.recovery: must be an object")
	ok("snapshot", obj{"snapshot": obj{"name": "snap-1", "xid": 0}})
	ok("snapshot json", obj{"snapshot": obj{"name": "s", "xid": json.Number("12345678901")}})
	bad("snapshot name empty", obj{"snapshot": obj{"name": "", "xid": 1}}, "payload.snapshot.name: must not be empty")
	bad("snapshot name missing", obj{"snapshot": obj{"xid": 1}}, "payload.snapshot.name: required field is missing")
	bad("snapshot xid negative", obj{"snapshot": obj{"name": "s", "xid": -1}}, "payload.snapshot.xid: below the minimum 0")
	bad("snapshot xid missing", obj{"snapshot": obj{"name": "s"}}, "payload.snapshot.xid: required field is missing")
	ok("deleted", obj{"deleted": obj{"source": "tombstone", "other": 1}})
	bad("deleted source empty", obj{"deleted": obj{"source": ""}}, "payload.deleted.source: must not be empty")
	bad("deleted source missing", obj{"deleted": obj{}}, "payload.deleted.source: required field is missing")

	// every call returns fresh values: a caller that changes one cannot affect the next
	a, b := common.CommonFields(), common.CommonFields()
	a[0].Name = "changed"
	a[1].Obj.Fields[0].Enum[0] = "changed"
	*a[2].Obj.Fields[1].Min = 99
	if b[0].Name != "raw" || common.CommonFields()[1].Obj.Fields[0].Enum[0] != "absent-from-live" || *common.CommonFields()[2].Obj.Fields[1].Min != 0 {
		t.Error("CommonFields shares state between calls")
	}
}

// TestSchemaErrorsNeverContainValues: a device controls the values, so no error
// may repeat one.
func TestSchemaErrorsNeverContainValues(t *testing.T) {
	const s1, s2, s3, s4 = "SENTINEL-alpha-7741", "SENTINEL-beta-9902", "SENTINEL-gamma-3318", "SENTINEL-delta-5560"
	role := &common.Schema{Fields: []common.Field{
		{Name: "role", Kind: common.KEnum, Enum: []string{"from", "to"}, Required: true},
		{Name: "address", Kind: common.KString, NonEmpty: true, MaxLen: 8},
		{Name: "id", Kind: common.KIDString},
		{Name: "n", Kind: common.KInt, Min: i64(0)},
		{Name: "when", Kind: common.KDate},
		{Name: "sha", Kind: common.KHex64},
		{Name: "tok", Kind: common.KToken},
	}}
	schema := common.Schema{
		Fields: append([]common.Field{
			{Name: "participants", Kind: common.KArray, Obj: role},
			{Name: "ids", Kind: common.KStringArray},
			{Name: "num", Kind: common.KNumber, Min: i64(0)},
			{Name: "flag", Kind: common.KBool},
			{Name: "thread", Kind: common.KObject, Obj: role},
		}, common.CommonFields()...),
		AtLeastOne: []string{"never_" + s1[:4]},
	}
	payloads := []obj{
		{"participants": []any{obj{"role": s1}}},
		{"participants": []any{obj{"role": "from", "address": s2 + s2}}},
		{"participants": []any{obj{"role": "from", "id": 1.5, "n": s3}}},
		{"participants": []any{obj{"role": "from", "n": json.Number("-9" + strings.Repeat("9", 6))}}},
		{"participants": []any{obj{"role": "to", "when": s4}}},
		{"participants": []any{obj{"role": "to", "sha": s1 + s2}}},
		{"participants": []any{obj{"role": "to", "tok": s3}}},
		{"participants": []any{s1}},
		{"ids": []any{"ok", s2}},
		{"ids": []any{json.Number("77" + "41")}},
		{"num": s3}, {"num": json.Number(s4)}, {"flag": s1},
		{"thread": s2}, {"thread": obj{"role": s3}},
		{"raw": s1}, {"raw": obj{s2: obj{s3: obj{s4: obj{s1: 1}}}}}, {"raw": obj{"k": struct{ s string }{s2}}},
		{"recovery": obj{"relation": s1}}, {"recovery": obj{s2: obj{s3: obj{s4: 1}}}},
		{"snapshot": obj{"name": 1, "xid": s3}}, {"snapshot": obj{"name": "n", "xid": json.Number("-" + "5" + "5")}},
		{"deleted": obj{"source": s4 + "x", "extra": s1}}, {"deleted": s2},
		{s1: s2, "unknown": s3},
	}
	sentinels := []string{s1, s2, s3, s4, "7741", "9902", "3318", "5560"}
	for i, p := range payloads {
		err := schema.Validate(p)
		if err == nil {
			t.Errorf("payload %d was accepted", i)
			continue
		}
		for _, s := range sentinels {
			if strings.Contains(err.Error(), s) {
				t.Errorf("payload %d: error %q contains the value %q", i, err, s)
			}
		}
	}
	// the Cross error is the type author's; the checker adds only the path
	err := common.Schema{Cross: func(map[string]any) error { return errors.New("rule broken") }}.Validate(obj{"k": s1})
	if err == nil || strings.Contains(err.Error(), s1) {
		t.Errorf("cross error = %v", err)
	}
}
