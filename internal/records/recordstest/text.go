package recordstest

import (
	"strings"

	"github.com/rbenzing/minutiae/internal/records"
)

// u builds a string from code points. The corpus below holds text that must not be typed as
// literals (combining marks, bidi controls, zero-width characters look identical to their
// neighbours in an editor), so every non-ASCII character is spelled as its code point.
func u(cps ...rune) string { return string(cps) }

// TextNames lists the names of the TextRecords corpus, in record order.
var TextNames = []string{
	"cafe_nfc", "cafe_nfd", "strasse", "fullwidth", "cjk", "phone", "url", "emoji", "bidi", "zerowidth",
	"turkish", "operators", "empty", "bodyonly", "summaryonly", "long",
}

// LongNeedle is the word at the very end of the 1 MiB body of the `long` record.
const LongNeedle = "needleattheend"

// TextRecords returns the deterministic full-text corpus for artifactID: 16 records of type note, one
// per name of TextNames, in that order. byName maps each name to the index of its record in recs;
// ingested into an empty case the record id is that index plus one. Every record has text except
// `empty` (summary "", no body), which the index does not hold. The records are:
//
//	cafe_nfc   "cafe au lait" with the precomposed e acute (U+00E9)
//	cafe_nfd   the same with e + combining acute (U+0065 U+0301)
//	strasse    "Strasse 12" spelled with a sharp s (U+00DF)
//	fullwidth  fullwidth ABC123 (U+FF21..U+FF23, U+FF11..U+FF13)
//	cjk        Chinese "hello world" (U+4F60 U+597D U+4E16 U+754C), " and ", Japanese "Tokyo Tower" (U+6771 U+4EAC U+30BF U+30EF U+30FC)
//	phone      "+1 (555) 123-4567"
//	url        "https://example.com/path?x=1&y=2"
//	emoji      an emoji with U+FE0F and one without
//	bidi       "pass" U+202E "word" (a right-to-left override inside a word)
//	zerowidth  "pass" U+200B "word" (a zero width space inside a word)
//	turkish    dotted capital I (U+0130) "stanbul and " dotless i (U+0131) "sp" U+0131 "rta"
//	operators  `NEAR(a b) OR "x" col:y -z a*b` (FTS5 syntax typed as text)
//	empty      summary "", body nil
//	bodyonly   summary "", the text only in the body
//	summaryonly the text only in the summary
//	long       a 1 MiB body whose last word is LongNeedle
func TextRecords(artifactID string) (recs []records.Record, byName map[string]int) {
	long := strings.Repeat("lorem ipsum ", (1<<20)/12+1)[:(1<<20)-len(LongNeedle)-1] + " " + LongNeedle
	texts := map[string][2]string{ // name -> summary, body
		"cafe_nfc":    {"caf" + u(0xe9) + " au lait", ""},
		"cafe_nfd":    {"cafe" + u(0x301) + " au lait", ""},
		"strasse":     {"Stra" + u(0xdf) + "e 12", ""},
		"fullwidth":   {u(0xff21, 0xff22, 0xff23, 0xff11, 0xff12, 0xff13), ""},
		"cjk":         {u(0x4f60, 0x597d, 0x4e16, 0x754c) + " and " + u(0x6771, 0x4eac, 0x30bf, 0x30ef, 0x30fc), ""},
		"phone":       {"+1 (555) 123-4567", ""},
		"url":         {"https://example.com/path?x=1&y=2", ""},
		"emoji":       {"party " + u(0x1f389) + " and love " + u(0x2764, 0xfe0f) + " and " + u(0x2764), ""},
		"bidi":        {"pass" + u(0x202e) + "word", ""},
		"zerowidth":   {"pass" + u(0x200b) + "word", ""},
		"turkish":     {u(0x130) + "stanbul and " + u(0x131) + "sp" + u(0x131) + "rta", ""},
		"operators":   {`NEAR(a b) OR "x" col:y -z a*b`, ""},
		"empty":       {"", ""},
		"bodyonly":    {"", "only the body holds bodyneedle"},
		"summaryonly": {"only the summary holds summaryneedle", ""},
		"long":        {"long record", long},
	}
	byName = make(map[string]int, len(TextNames))
	for i, name := range TextNames {
		tx := texts[name]
		byName[name] = i
		recs = append(recs, records.Record{
			Type: "note", ArtifactID: artifactID, Summary: tx[0], Body: tx[1],
			Payload: map[string]any{"name": name},
		})
	}
	return recs, byName
}
