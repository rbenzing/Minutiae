package common_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

func TestCleanTextFlagsAndPreserves(t *testing.T) {
	t.Run("valid text is untouched and unflagged", func(t *testing.T) {
		for _, s := range []string{"", "hello", "héllo wörld €", "日本語", "a\tb\nc", "emoji 😀", "already \uFFFD valid"} {
			c := common.CleanText([]byte(s), 1<<20)
			if c.Text != s || len(c.Flags) != 0 || c.RawB64 != "" || c.RawSHA256 != "" || c.Truncated || c.TotalBytes != 0 {
				t.Errorf("CleanText(%q) = %+v, want it untouched with no flags", s, c)
			}
			if c.RawLen != len(s) {
				t.Errorf("RawLen = %d, want %d", c.RawLen, len(s))
			}
			if f := c.PayloadFields(); len(f) != 0 {
				t.Errorf("clean text produced payload fields %v", f)
			}
		}
	})
	t.Run("NFD text and bidi controls survive", func(t *testing.T) {
		for _, s := range []string{
			"e\u0301cole A\u030Angstro\u0308m",         // decomposed accents are not normalized
			"abc \u202Edef\u202C \u2066x\u2069 \u200F", // bidi overrides, isolates and marks
			"zero\u200Bwidth\uFEFFjoiner\u200D",
		} {
			c := common.CleanText([]byte(s), 1<<20)
			if c.Text != s || len(c.Flags) != 0 {
				t.Errorf("CleanText(%q) = %+v, want it unchanged", s, c)
			}
		}
	})
	cases := []struct {
		name  string
		raw   []byte
		text  string
		flags []string
	}{
		{"invalid byte", []byte("ab\xffcd"), "ab\uFFFDcd", []string{"invalid_utf8"}},
		{"truncated sequence", []byte("ab\xe2\x82"), "ab\uFFFD\uFFFD", []string{"invalid_utf8"}},
		{"overlong and surrogate", []byte("\xc0\xaf\xed\xa0\x80"), "\uFFFD\uFFFD\uFFFD\uFFFD\uFFFD", []string{"invalid_utf8"}},
		{"NUL", []byte("a\x00b"), "a\uFFFDb", []string{"nul"}},
		{"both", []byte("a\x00b\xffc"), "a\uFFFDb\uFFFDc", []string{"invalid_utf8", "nul"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := common.CleanText(tc.raw, 1<<20)
			if c.Text != tc.text || !slices.Equal(c.Flags, tc.flags) {
				t.Fatalf("text %q flags %v, want %q %v", c.Text, c.Flags, tc.text, tc.flags)
			}
			if !utf8.ValidString(c.Text) || strings.ContainsRune(c.Text, 0) {
				t.Errorf("text is not valid UTF-8 without NUL: %q", c.Text)
			}
			back, err := base64.StdEncoding.DecodeString(c.RawB64)
			if err != nil || !bytes.Equal(back, tc.raw) {
				t.Errorf("RawB64 decodes to %q (%v), want the original %q", back, err, tc.raw)
			}
			if c.RawSHA256 != "" || c.RawLen != len(tc.raw) {
				t.Errorf("RawSHA256 %q RawLen %d", c.RawSHA256, c.RawLen)
			}
		})
	}
	t.Run("a large original is recorded by hash", func(t *testing.T) {
		raw := append(bytes.Repeat([]byte("x"), 100<<10), 0xff)
		c := common.CleanText(raw, 1<<30)
		sum := sha256.Sum256(raw)
		if c.RawB64 != "" || c.RawSHA256 != fmt.Sprintf("%x", sum) || c.RawLen != len(raw) {
			t.Errorf("RawB64 %d bytes, RawSHA256 %q, RawLen %d", len(c.RawB64), c.RawSHA256, c.RawLen)
		}
		f := c.PayloadFields()
		if _, ok := f["body_raw_b64"]; ok || f["body_raw_sha256"] != c.RawSHA256 || f["body_raw_len"] != int64(len(raw)) {
			t.Errorf("payload fields = %v", f)
		}
	})
	t.Run("the 64 KiB boundary", func(t *testing.T) {
		at := append(bytes.Repeat([]byte("x"), 64<<10-1), 0xff)
		if c := common.CleanText(at, 1<<30); c.RawB64 == "" || c.RawSHA256 != "" {
			t.Error("a 64 KiB original must be kept as base64")
		}
		over := append(bytes.Repeat([]byte("x"), 64<<10), 0xff)
		if c := common.CleanText(over, 1<<30); c.RawB64 != "" || c.RawSHA256 == "" {
			t.Error("a 64 KiB + 1 original must be recorded by hash")
		}
	})
	t.Run("flags are found in the part past the cut", func(t *testing.T) {
		raw := append(bytes.Repeat([]byte("a"), 100), 0xff, 0x00)
		c := common.CleanText(raw, 10)
		if c.Text != strings.Repeat("a", 10) || !slices.Equal(c.Flags, []string{"invalid_utf8", "nul", "truncated"}) || c.RawB64 == "" {
			t.Errorf("CleanText = %+v", c)
		}
	})
}

// TestOversizeBodyIsMarkedTruncated: the cut is on a rune boundary at every
// offset around the cap, for runes of 1 to 4 bytes.
func TestOversizeBodyIsMarkedTruncated(t *testing.T) {
	for _, r := range []rune{'a', 'é', '€', '😀'} {
		size := utf8.RuneLen(r)
		raw := []byte(strings.Repeat(string(r), 20))
		for limit := 0; limit <= len(raw)+3; limit++ {
			c := common.CleanText(raw, limit)
			if !utf8.ValidString(c.Text) {
				t.Fatalf("rune %q limit %d: cut inside a rune: %q", r, limit, c.Text)
			}
			if len(c.Text) > limit {
				t.Fatalf("rune %q limit %d: text of %d bytes exceeds the cap", r, limit, len(c.Text))
			}
			wantLen := limit / size * size
			if limit >= len(raw) {
				wantLen = len(raw)
			}
			if len(c.Text) != wantLen {
				t.Errorf("rune %q limit %d: %d bytes kept, want %d", r, limit, len(c.Text), wantLen)
			}
			if truncated := limit < len(raw); c.Truncated != truncated {
				t.Errorf("rune %q limit %d: Truncated = %v", r, limit, c.Truncated)
			} else if truncated {
				if c.TotalBytes != len(raw) || !slices.Contains(c.Flags, "truncated") || c.RawB64 != "" {
					t.Errorf("rune %q limit %d: %+v", r, limit, c)
				}
				f := c.PayloadFields()
				if f["body_truncated"] != true || f["body_total_bytes"] != int64(len(raw)) {
					t.Errorf("payload fields = %v", f)
				}
			} else if len(c.Flags) != 0 || c.TotalBytes != 0 {
				t.Errorf("rune %q limit %d: %+v, want no flags", r, limit, c)
			}
		}
	}
	t.Run("a cap of zero or less is zero", func(t *testing.T) {
		for _, limit := range []int{0, -1, -1 << 30} {
			c := common.CleanText([]byte("hello"), limit)
			if c.Text != "" || !c.Truncated || c.TotalBytes != 5 || !slices.Equal(c.Flags, []string{"truncated"}) {
				t.Errorf("limit %d: %+v", limit, c)
			}
			e := common.CleanText(nil, limit)
			if e.Text != "" || e.Truncated || len(e.Flags) != 0 {
				t.Errorf("limit %d, empty input: %+v", limit, e)
			}
		}
	})
	t.Run("replacement characters count toward the cap", func(t *testing.T) {
		c := common.CleanText([]byte("\xff\xff\xff\xff"), 7) // each becomes a 3-byte U+FFFD
		if c.Text != "\uFFFD\uFFFD" || !c.Truncated || c.TotalBytes != 4 {
			t.Errorf("CleanText = %+v", c)
		}
	})
	t.Run("nil input", func(t *testing.T) {
		if c := common.CleanText(nil, 10); c.Text != "" || c.Truncated || c.RawLen != 0 {
			t.Errorf("CleanText(nil) = %+v", c)
		}
	})
}

// TestPayloadFieldsUsesCanonicalTypes: every value is one the records package
// canonicalizes, and a real writer accepts a record that carries the fields.
func TestPayloadFieldsUsesCanonicalTypes(t *testing.T) {
	inputs := [][]byte{
		[]byte("clean"),
		[]byte("a\x00b\xff"),
		bytes.Repeat([]byte("é"), 200),
		append(bytes.Repeat([]byte("x"), 100<<10), 0x00),
	}
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "a.db", make([]byte, 1<<20))
	var recs []records.Record
	for _, raw := range inputs {
		cl := common.CleanText(raw, 100)
		f := cl.PayloadFields()
		for k, v := range f {
			if !canonical(v) {
				t.Errorf("%s holds a %T", k, v)
			}
		}
		payload := map[string]any{"channel": "sms"}
		for k, v := range f {
			payload[k] = v
		}
		recs = append(recs, records.Record{Type: "note", ArtifactID: a.ID, Summary: common.Summarize(cl.Text, 80), Body: cl.Text, Payload: payload})
	}
	w, err := records.NewWriter(c, records.Parser{Name: "common-test", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	for i, r := range recs {
		if err := w.Add(ctx, r); err != nil {
			t.Fatalf("record %d refused by the writer: %v", i, err)
		}
	}
	if res, err := w.End(ctx); err != nil || res.Records != int64(len(recs)) || res.Rejected != 0 {
		t.Fatalf("End = %+v, %v", res, err)
	}
}

// canonical reports whether v is a string, bool, int64, []any or map[string]any
// (recursively).
func canonical(v any) bool {
	switch x := v.(type) {
	case string, bool, int64:
		return true
	case []any:
		for _, e := range x {
			if !canonical(e) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, e := range x {
			if !canonical(e) {
				return false
			}
		}
		return true
	}
	return false
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"empty", "", 10, ""},
		{"fits", "hello", 10, "hello"},
		{"newlines and tabs", "a\nb\t\tc\r\nd", 50, "a b c d"},
		{"controls become spaces", "a\x00b\x01\x02c\x7fd", 50, "a b c d"},
		{"unicode spaces", "a\u00A0\u2003b\u2028c", 50, "a b c"},
		{"trimmed", "  \n hello \t ", 50, "hello"},
		{"only whitespace", " \n\t\x00 ", 50, ""},
		{"exactly max", "abcdef", 6, "abcdef"},
		{"cut with an ellipsis", "abcdefghij", 8, "abcde..."},
		{"multi-byte cut", "ééééé", 8, "éé..."}, // 3 bytes left for "..." and the rune boundary
		{"cut before a 3-byte rune", "ab€€€", 7, "ab..."},
		{"four-byte runes", "😀😀😀😀", 10, "😀..."},
		{"trailing space before the dots is dropped", "abcd efghijk", 8, "abcd..."},
		{"max of 3", "abcdef", 3, "..."},
		{"max under 3", "abcdef", 2, ".."},
		{"max 1", "abcdef", 1, "."},
		{"max 0", "abc", 0, ""},
		{"negative max", "abc", -5, ""},
		{"invalid UTF-8", "a\xffb", 10, "a\uFFFDb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := common.Summarize(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("Summarize(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if len(got) > max(tc.max, 0) || !utf8.ValidString(got) || strings.ContainsAny(got, "\n\r\t\x00") {
				t.Errorf("result %q breaks the one-line, valid, bounded contract", got)
			}
		})
	}
	t.Run("a huge text costs bounded work and stays bounded", func(t *testing.T) {
		got := common.Summarize(strings.Repeat("word \n", 1<<20), 64)
		if len(got) > 64 || !strings.HasSuffix(got, "...") {
			t.Errorf("got %d bytes %q", len(got), got)
		}
	})
}

// TestHugeLimitsAreClamped: limit+UTFMax must not overflow, so neither CleanText
// nor Summarize panics or silently cuts at a limit near the int maximum.
func TestHugeLimitsAreClamped(t *testing.T) {
	for _, limit := range []int{math.MaxInt, math.MaxInt - 1, math.MaxInt - utf8.UTFMax, math.MaxInt - 3, math.MaxInt32, 1 << 40} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			raw := []byte("hello \xff world é\x00 end")
			c := common.CleanText(raw, limit)
			if want := "hello � world é� end"; c.Text != want || c.Truncated || !slices.Equal(c.Flags, []string{"invalid_utf8", "nul"}) {
				t.Errorf("CleanText limit %d = %+v", limit, c)
			}
			if got := common.Summarize("hello   world\nand more", limit); got != "hello world and more" {
				t.Errorf("Summarize limit %d = %q", limit, got)
			}
		})
	}
	t.Run("a long text under a huge limit is not cut", func(t *testing.T) {
		raw := bytes.Repeat([]byte("abcdefgh"), 1<<14)
		if c := common.CleanText(raw, math.MaxInt); c.Truncated || len(c.Text) != len(raw) {
			t.Errorf("text cut: truncated %v, %d bytes of %d", c.Truncated, len(c.Text), len(raw))
		}
		if got := common.Summarize(strings.Repeat("word ", 1<<14), math.MaxInt); len(got) != 5*(1<<14)-1 {
			t.Errorf("Summarize cut a text that fits: %d bytes", len(got))
		}
	})
}

// TestSummarizeShowsInvisibleFormatting: bidi controls and zero-width characters
// (category Cf) are shown as <U+XXXX> in the one-line summary, so a summary can
// neither reorder nor hide what it shows, and "Bank" and "Ba<ZWSP>nk" differ. The
// stored body keeps them untouched.
func TestSummarizeShowsInvisibleFormatting(t *testing.T) {
	// code points are built from numbers so this file holds no invisible characters
	cp := func(parts ...any) string {
		var b strings.Builder
		for _, p := range parts {
			switch v := p.(type) {
			case string:
				b.WriteString(v)
			case rune:
				b.WriteRune(v)
			}
		}
		return b.String()
	}
	const (
		rlo, pdf        = rune(0x202E), rune(0x202C)
		lre, rle, lro   = rune(0x202A), rune(0x202B), rune(0x202D)
		lri, rli, fsi   = rune(0x2066), rune(0x2067), rune(0x2068)
		pdi             = rune(0x2069)
		lrm, rlm, alm   = rune(0x200E), rune(0x200F), rune(0x061C)
		zwsp, zwnj, zwj = rune(0x200B), rune(0x200C), rune(0x200D)
		wordJoiner, bom = rune(0x2060), rune(0xFEFF)
		softHyphen      = rune(0x00AD)
	)
	cases := []struct{ name, in, want string }{
		{"right-to-left override", cp("pay ", rlo, "txt.exe", pdf, " now"), "pay <U+202E>txt.exe<U+202C> now"},
		{"embeddings and isolates", cp("a", lre, rle, lro, lri, rli, fsi, "b", pdi, "c"), "a<U+202A><U+202B><U+202D><U+2066><U+2067><U+2068>b<U+2069>c"},
		{"marks", cp("a", lrm, rlm, "b", alm, "c"), "a<U+200E><U+200F>b<U+061C>c"},
		{"zero width", cp("pa", zwsp, "ss", zwnj, "wo", zwj, "rd", wordJoiner, bom, "!"), "pa<U+200B>ss<U+200C>wo<U+200D>rd<U+2060><U+FEFF>!"},
		{"soft hyphen", cp("co", softHyphen, "op"), "co<U+00AD>op"},
		{"only invisible", cp(zwsp, rlo, wordJoiner), "<U+200B><U+202E><U+2060>"},
		{"space kept around them", cp("a ", rlo, " b"), "a <U+202E> b"},
		{"tag character (five hex digits)", cp("a", rune(0xE0001), "b"), "a<U+E0001>b"},
		{"ordinary text is untouched", "héllo wörld 日本語 😀", "héllo wörld 日本語 😀"},
	}
	for _, tc := range cases {
		if got := common.Summarize(tc.in, 100); got != tc.want {
			t.Errorf("%s: Summarize(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if got := common.SummarizeChars(tc.in, 100); got != tc.want {
			t.Errorf("%s: SummarizeChars(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	// a look-alike with an invisible character is not the same summary as the plain word
	if common.Summarize("Bank", 50) == common.Summarize(cp("Ba", zwsp, "nk"), 50) {
		t.Error("\"Bank\" and a Bank with a zero-width space give the same summary")
	}
	// the escape is never cut in half, whichever way the limit is counted
	if got := common.Summarize(cp(zwsp, zwsp, zwsp, zwsp, zwsp, zwsp, zwsp, zwsp, zwsp, zwsp), 20); got != "<U+200B><U+200B>..." {
		t.Errorf("Summarize cut inside an escape: %q", got)
	}
	if got := common.SummarizeChars(cp(zwsp, zwsp, zwsp, zwsp), 20); got != "<U+200B><U+200B>..." {
		t.Errorf("SummarizeChars cut inside an escape: %q", got)
	}
	// the stored text is not changed
	in := cp("pay ", rlo, "txt.exe", pdf)
	if c := common.CleanText([]byte(in), 100); c.Text != in || len(c.Flags) != 0 {
		t.Errorf("CleanText changed formatting characters: %+v", c)
	}
}

// TestSummarizeChars: the limit counts characters (runes of the result), not bytes,
// so CJK and emoji text keeps the same number of characters as ASCII; the result is
// still one valid line and an escape counts as the characters it shows.
func TestSummarizeChars(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"empty", "", 5, ""},
		{"fits in characters though not in bytes", "日本語日本", 5, "日本語日本"},
		{"cut by characters", "日本語日本語日本語", 5, "日本..."},
		{"two-byte letters", "héllo wörld", 8, "héllo..."},
		{"emoji", "😀😀😀😀😀😀", 5, "😀😀..."},
		{"exactly max", "abcdef", 6, "abcdef"},
		{"one over", "abcdefg", 6, "abc..."},
		{"collapses and trims", "  a\n\tb \x00 c  ", 20, "a b c"},
		{"max of 3", "abcdef", 3, "..."},
		{"max under 3", "abcdef", 2, ".."},
		{"max 0", "abc", 0, ""},
		{"negative", "abc", -1, ""},
		{"invalid UTF-8", "a\xffb", 10, "a" + string(rune(0xFFFD)) + "b"},
		{"trailing space before the dots is dropped", "abcd efghijk", 8, "abcd..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := common.SummarizeChars(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("SummarizeChars(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n > max(tc.max, 0) || !utf8.ValidString(got) || strings.ContainsAny(got, "\n\r\t\x00") {
				t.Errorf("result %q (%d characters) breaks the one-line, valid, bounded contract", got, n)
			}
		})
	}
	// 80 characters of CJK is 80 characters, not 26
	long := common.SummarizeChars(strings.Repeat("日本語", 100), 80)
	if utf8.RuneCountInString(long) != 80 || !strings.HasSuffix(long, "...") {
		t.Errorf("long CJK summary = %q (%d characters)", long, utf8.RuneCountInString(long))
	}
	// huge limits are clamped and huge texts cost bounded work
	if got := common.SummarizeChars("hello   world", math.MaxInt); got != "hello world" {
		t.Errorf("SummarizeChars with MaxInt = %q", got)
	}
	if got := common.SummarizeChars(strings.Repeat("word \n", 1<<20), 64); utf8.RuneCountInString(got) != 64 || !strings.HasSuffix(got, "...") {
		t.Errorf("a huge text gave %d characters", utf8.RuneCountInString(got))
	}
}
