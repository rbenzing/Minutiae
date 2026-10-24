package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// encodeJSON is the plain encoding/json output the safe writer starts from.
func encodeJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTerminalSafeJSONEscapesEveryNonGraphicRune(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		in := "a" + string(r) + "b"
		out, err := marshalSafe(in)
		if err != nil {
			t.Fatalf("U+%04X: %v", r, err)
		}
		for _, o := range string(out) {
			if !safeRune(o) {
				t.Fatalf("U+%04X: output %q holds the raw rune U+%04X", r, out, o)
			}
		}
		var back string
		if err := json.Unmarshal(out, &back); err != nil || back != in {
			t.Fatalf("U+%04X: decoded %q (%v), want %q", r, back, err, in)
		}
		if r >= 0x80 && unicode.IsGraphic(r) && !strings.Contains(string(out), string(r)) {
			t.Fatalf("U+%04X is graphic but was escaped: %q", r, out)
		}
	}
}

func TestTerminalSafeJSONPinnedOutputs(t *testing.T) {
	for _, tc := range []struct {
		r    rune
		want string
	}{
		{0x202e, `"a\u202eb"`},
		{0x85, `"a\u0085b"`},
		{0x7f, `"a\u007fb"`},
		{0x2066, `"a\u2066b"`},
		{0xe0001, `"a\udb40\udc01b"`},
		{0x1f600, "\"a\U0001f600b\""},
		{0xe9, "\"a\u00e9b\""},
		{0x301, "\"a\u0301b\""},
		{0xa0, "\"a\u00a0b\""},
		{0xe000, `"a\ue000b"`},
		{'<', "\"a\\" + "u003cb\""}, // encoding/json's HTML escaping is kept
	} {
		out, err := marshalSafe("a" + string(tc.r) + "b")
		if err != nil || string(out) != tc.want {
			t.Errorf("U+%04X: got %q (%v), want %q", tc.r, out, err, tc.want)
		}
	}
}

func TestTerminalSafeJSONIsIdentityOnGraphicOutput(t *testing.T) {
	v := map[string]any{
		"k \"q\"": []any{"a\\b", "line\nbreak", "tab\t", "caf\u00e9 \u4e16\u754c \U0001f600", json.RawMessage(`{"x": [1, 2.5, null, true]}`)},
		"nested":  map[string]any{"z": []string{"", "x\"y"}, "n": 3},
	}
	plain := encodeJSON(t, v)
	got, err := terminalSafeJSON(plain)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("graphic output changed: %q -> %q (%v)", plain, got, err)
	}
	hostile := encodeJSON(t, []string{"\u202e\u0085\x1b"})
	once, err := terminalSafeJSON(hostile)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := terminalSafeJSON(once)
	if err != nil || !bytes.Equal(once, twice) {
		t.Errorf("second pass changed %q -> %q (%v)", once, twice, err)
	}
}

func FuzzTerminalSafeJSON(f *testing.F) {
	for _, s := range []string{"", "a", "\u202e", "\x1b[31m", "\u0085", "\U000e0001", "\"\\", "caf\u00e9", "\u2028x", "\x7f"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			t.Skip()
		}
		in := map[string][]string{"k" + s: {s}}
		out, err := marshalSafe(in)
		if err != nil {
			t.Fatal(err)
		}
		var back map[string][]string
		if err := json.Unmarshal(out, &back); err != nil || len(back) != 1 || len(back["k"+s]) != 1 || back["k"+s][0] != s {
			t.Fatalf("decoded %v (%v), want %q", back, err, s)
		}
		for _, o := range string(out) {
			if !safeRune(o) {
				t.Fatalf("raw non-graphic U+%04X in %q", o, out)
			}
		}
		again, err := terminalSafeJSON(out)
		if err != nil || !bytes.Equal(again, out) {
			t.Fatalf("not a fixed point: %q -> %q (%v)", out, again, err)
		}
	})
}

func TestWriteJSONRefusesInvalidUTF8(t *testing.T) {
	var buf bytes.Buffer
	err := writeJSON(&buf, struct {
		Name string `json:"name"`
	}{"a\xffb"})
	if !errors.Is(err, errJSONNotUTF8) || !strings.Contains(err.Error(), "name") || buf.Len() != 0 {
		t.Errorf("string field: err %v, wrote %q", err, buf.String())
	}
	buf.Reset()
	err = writeJSON(&buf, map[string]int{"k\xff": 1})
	if !errors.Is(err, errJSONNotUTF8) || buf.Len() != 0 {
		t.Errorf("map key: err %v, wrote %q", err, buf.String())
	}
	buf.Reset()
	err = writeJSON(&buf, json.RawMessage("\"a\xffb\""))
	if !errors.Is(err, errJSONNotUTF8) || !strings.Contains(err.Error(), "byte") || buf.Len() != 0 {
		t.Errorf("raw message: err %v, wrote %q", err, buf.String())
	}
	if _, err := marshalSafe([]string{"ok", "a\xffb"}); !errors.Is(err, errJSONNotUTF8) || !strings.Contains(err.Error(), "[1]") {
		t.Errorf("slice element: err %v, want the path [1]", err)
	}
}

func TestWriteJSONIsIndentedAndTerminated(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, map[string]any{"a": []int{1}}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "{\n  \"a\": [\n    1\n  ]\n}\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNDJSONIsTerminalSafe(t *testing.T) {
	var b bytes.Buffer
	if err := writeNDJSON(&b, map[string]string{"x": "\u202e"}); err != nil {
		t.Fatal(err)
	}
	if got, want := b.String(), `{"x":"\u202e"}`+"\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriteJSONArtifactKeepsThePlainEncoding(t *testing.T) {
	var b bytes.Buffer
	if err := writeJSONArtifact(&b, map[string]string{"x": "\u202e"}); err != nil {
		t.Fatal(err)
	}
	if got, want := b.String(), "{\n  \"x\": \"\u202e\"\n}\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

type embeddedInner struct {
	Deep string `json:"deep"`
}

type embeddedOuter struct {
	embeddedInner
	Other string `json:"other"`
}

func TestWriteJSONPreCheckFollowsFieldRules(t *testing.T) {
	okCases := map[string]any{
		"json:- field": struct {
			Skip string `json:"-"`
			Kept string `json:"kept"`
		}{"\xff", "x"},
		"unexported field": struct {
			hidden string //nolint:unused // read by reflection only
			Kept   string `json:"kept"`
		}{"\xff", "x"},
		"empty omitempty": struct {
			S string `json:"s,omitempty"`
		}{""},
	}
	for name, v := range okCases {
		var buf bytes.Buffer
		if err := writeJSON(&buf, v); err != nil {
			t.Errorf("%s: refused: %v", name, err)
			continue
		}
		var plain bytes.Buffer
		enc := json.NewEncoder(&plain)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		if buf.String() != plain.String() {
			t.Errorf("%s: got %q, want the plain encoding %q", name, buf.String(), plain.String())
		}
	}
	refused := map[string]any{
		"promoted field": embeddedOuter{embeddedInner{"a\xffb"}, "ok"},
		"omitempty non-empty": struct {
			S string `json:"s,omitempty"`
		}{"\xff"},
	}
	for name, v := range refused {
		var buf bytes.Buffer
		err := writeJSON(&buf, v)
		if !errors.Is(err, errJSONNotUTF8) || buf.Len() != 0 {
			t.Errorf("%s: err %v, wrote %q", name, err, buf.String())
		}
	}
	err := writeJSON(&bytes.Buffer{}, embeddedOuter{embeddedInner{"a\xffb"}, "ok"})
	if err == nil || !strings.Contains(err.Error(), "deep") {
		t.Errorf("promoted field error %v does not name the field", err)
	}
}

func TestWriteJSONDepthCapIsTyped(t *testing.T) {
	nest := func(n int) any {
		var v any = "leaf"
		for range n {
			v = []any{v}
		}
		return v
	}
	var buf bytes.Buffer
	if err := writeJSON(&buf, nest(300)); !errors.Is(err, errJSONTooDeep) || buf.Len() != 0 {
		t.Errorf("300 deep: err %v, wrote %d bytes", err, buf.Len())
	}
	if err := writeJSON(&buf, nest(200)); err != nil {
		t.Errorf("200 deep: %v", err)
	}
}

func TestJoinJSON(t *testing.T) {
	if joinJSON(nil, nil) != nil {
		t.Error("joinJSON(nil, nil) is not nil")
	}
	if err := joinJSON(nil, errJSONNotUTF8); !errors.Is(err, errJSONNotUTF8) {
		t.Errorf("joinJSON(nil, e) = %v", err)
	}
}

// safeRune reports whether a rune may appear raw in terminal-safe JSON output.
func safeRune(o rune) bool {
	return (o >= 0x20 && o <= 0x7e) || (o >= 0x80 && unicode.IsGraphic(o))
}
