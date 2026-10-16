package sqlitedb_test

// NOCASE and RTRIM equality on text with embedded NUL bytes (LX-4 U56): the
// engine's NOCASE comparison stops at the first NUL of the common length, so
// 'a<NUL>b' and 'a<NUL>c' are equal, and the join must pair them. The engine is
// the oracle for every pair, in both directions.

import "testing"

func TestJoinOnTextWithEmbeddedNULMatchesTheEngine(t *testing.T) {
	vals := []any{
		"a\x00b", "a\x00c", "A\x00b", "a\x00B", "\x00", "a\x00", "a", "A", "a\x00\x00", "\x00a", "a\x00é", "a\x00É",
		"b\x00b", "ab", "a ", "a\x00 ", "a \x00",
	}
	for _, enc := range []string{"UTF-8", "UTF-16le"} {
		for _, gdecl := range []string{"text collate nocase", "text collate rtrim", "text"} {
			t.Run(enc+"/"+gdecl, func(t *testing.T) {
				data := engineTwoTables(t, enc, "text", gdecl, vals, vals)
				decided, _ := checkTargetSide(t, data, nil)
				if decided == 0 {
					t.Fatal("nothing was decided")
				}
			})
		}
	}
}
