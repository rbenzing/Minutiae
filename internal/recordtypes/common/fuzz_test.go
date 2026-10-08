package common_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// fuzzSchema exercises every kind, nested objects and arrays, AtLeastOne and Cross.
func fuzzSchema() common.Schema {
	elem := &common.Schema{Fields: []common.Field{
		{Name: "role", Kind: common.KEnum, Enum: []string{"from", "to", "cc"}, Required: true},
		{Name: "address", Kind: common.KString, NonEmpty: true, MaxLen: 64},
		{Name: "tags", Kind: common.KStringArray},
		{Name: "sub", Kind: common.KObject, Obj: &common.Schema{Fields: []common.Field{{Name: "x", Kind: common.KRaw}}}},
	}}
	zero := int64(0)
	return common.Schema{
		Fields: append([]common.Field{
			{Name: "channel", Kind: common.KToken, Required: true},
			{Name: "participants", Kind: common.KArray, Obj: elem},
			{Name: "id", Kind: common.KIDString},
			{Name: "n", Kind: common.KInt, Min: &zero},
			{Name: "x", Kind: common.KNumber, Min: &zero},
			{Name: "sha", Kind: common.KHex64},
			{Name: "day", Kind: common.KDate},
			{Name: "flag", Kind: common.KBool},
			{Name: "list", Kind: common.KArray},
			{Name: "thread", Kind: common.KObject},
		}, common.CommonFields()...),
		AtLeastOne: []string{"participants", "id"},
		Cross: func(m map[string]any) error {
			if _, ok := m["flag"]; ok && m["id"] == nil {
				return errors.New("cross rule")
			}
			return nil
		},
	}
}

// FuzzSchemaValidate: any JSON-decoded payload is validated without a panic, with
// bounded work, and an error never repeats a string value of the payload.
func FuzzSchemaValidate(f *testing.F) {
	for _, seed := range []string{
		`{}`, `{"channel":"sms","participants":[{"role":"from","address":"+15551234567"}],"id":7}`,
		`{"channel":"SMS"}`, `{"participants":[{"role":"owner"}],"id":"x"}`,
		`{"channel":"sms","id":1,"n":-1,"x":1e999,"sha":"ABC","day":"2024-02-30"}`,
		`{"id":1,"raw":{"a":{"b":{"c":1}}},"recovery":{"relation":"found","w":{"a":{"b":1}}}}`,
		`{"id":1,"snapshot":{"name":"","xid":-1},"deleted":{"source":""}}`,
		`{"id":18446744073709551615,"n":9223372036854775808,"x":0x10}`,
		`{"id":1,"list":[[[[1]]]],"thread":{"a":[{"b":[{"c":1}]}]}}`,
		`{"participants":[{"role":"to","tags":[1,"a",null],"sub":{"x":{"y":{"z":{}}}}}],"id":1}`,
		`[1,2]`, `null`, `"x"`, `{"id":"` + strings.Repeat("a", 2000) + `"}`,
	} {
		f.Add([]byte(seed))
	}
	schema := fuzzSchema()
	f.Fuzz(func(t *testing.T, data []byte) {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			return
		}
		m, _ := v.(map[string]any)
		err := schema.Validate(m)
		if err == nil {
			return
		}
		msg := err.Error()
		if !utf8.ValidString(msg) || len(msg) > 4096 {
			t.Fatalf("error text is not a short valid string: %q", msg)
		}
		// no string value of the payload may appear in the error
		var walk func(v any)
		walk = func(v any) {
			switch x := v.(type) {
			case string:
				if len(x) >= 12 && strings.Contains(msg, x) {
					t.Fatalf("error %q repeats the payload value %q", msg, x)
				}
			case []any:
				for _, e := range x {
					walk(e)
				}
			case map[string]any:
				for _, e := range x {
					walk(e)
				}
			}
		}
		walk(m)
	})
}

// FuzzCleanText: the text is valid UTF-8 without NUL, within the cap, and the flags
// and the kept original are consistent.
func FuzzCleanText(f *testing.F) {
	for _, s := range []string{"", "plain", "a\x00b", "\xff\xfe", "é€😀", "\xe2\x82", strings.Repeat("x", 300)} {
		f.Add([]byte(s), 10)
	}
	f.Fuzz(func(t *testing.T, raw []byte, limit int) {
		limit %= 1 << 12
		c := common.CleanText(raw, limit)
		if !utf8.ValidString(c.Text) || strings.ContainsRune(c.Text, 0) {
			t.Fatalf("text %q is not valid UTF-8 without NUL", c.Text)
		}
		if limit < 0 {
			limit = 0
		}
		if len(c.Text) > limit {
			t.Fatalf("%d bytes over the cap of %d", len(c.Text), limit)
		}
		if c.RawLen != len(raw) {
			t.Fatalf("RawLen = %d, want %d", c.RawLen, len(raw))
		}
		if (c.RawB64 != "" || c.RawSHA256 != "") && len(c.Flags) == 0 {
			t.Fatal("the original is kept although nothing was flagged")
		}
		if c.Truncated != (c.TotalBytes != 0) && len(raw) != 0 {
			t.Fatalf("Truncated %v, TotalBytes %d", c.Truncated, c.TotalBytes)
		}
		if utf8.Valid(raw) && !bytes.Contains(raw, []byte{0}) && len(raw) <= limit && (c.Text != string(raw) || len(c.Flags) != 0) {
			t.Fatalf("clean text changed: %q -> %+v", raw, c)
		}
		if s := common.Summarize(string(raw), limit); len(s) > max(limit, 0) || !utf8.ValidString(s) || strings.ContainsAny(s, "\n\r\t\x00") {
			t.Fatalf("Summarize(limit %d) = %q breaks its contract", limit, s)
		}
		if s := common.SummarizeChars(string(raw), limit); utf8.RuneCountInString(s) > max(limit, 0) || !utf8.ValidString(s) || strings.ContainsAny(s, "\n\r\t\x00") {
			t.Fatalf("SummarizeChars(limit %d) = %q breaks its contract", limit, s)
		}
	})
}
