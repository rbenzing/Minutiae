package evidence

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// FTSPipelineVersion is bumped by hand whenever NormalizeText changes behaviour: it is part of
// FTSNormVersion, so a changed pipeline makes every existing index "not current" until it is
// rebuilt with `records reindex`.
const FTSPipelineVersion = 3

// xTextVersion is the golang.org/x/text module version this build is made with. The index content
// depends on its tables (NFKC, case folding), not only on the Unicode version they implement, so it
// is part of FTSNormVersion. TestXTextVersionMatchesGoMod fails when it differs from go.mod, so an
// x/text upgrade can never change index content without this constant (and the tests that pin the
// normalization) being revisited.
const xTextVersion = "v0.42.0"

const (
	runeZeroWidthSpace = 0x200b // ZERO WIDTH SPACE: a word separator in Thai and Khmer

	runeDotlessI     = 0x0131 // LATIN SMALL LETTER DOTLESS I
	runeCombiningDot = 0x0307 // COMBINING DOT ABOVE
)

// NormalizeText returns the text the full-text indexes hold and queries are compiled from
// (pipeline version 3):
//
//  1. invalid UTF-8 becomes U+FFFD (one per undecodable byte); NUL becomes U+0020;
//  2. U+200B ZERO WIDTH SPACE becomes U+0020 (it separates words in Thai and Khmer); every other
//     Cf rune (bidi controls, U+200C, U+200D and U+2060, which join within words, soft hyphen,
//     BOM) and every variation selector (U+FE00-U+FE0F, U+E0100-U+E01EF; so an emoji with and
//     without U+FE0F meet) is dropped; every Cc, Zs, Zl and Zp rune becomes U+0020;
//  3. NFKC; 4. full Unicode case folding; 5. NFKC again;
//  6. the deliberate mappings the folding does not give: U+0131 becomes "i", a run of U+0307
//     after an "i" is dropped (the Turkish ones, for recall), and Cherokee letters are mapped to
//     their small forms (the folding of x/text swaps capital and small Cherokee letters, so
//     without this the two spellings of one letter would not meet);
//  7. NFKC again (step 6 can leave a composable pair), then step 2 once more as a guard (so the
//     output contract does not depend on what a Unicode table maps to), and finally every run of
//     U+0020 collapses to one and the leading and trailing U+0020 are trimmed (the same for stored
//     text and for a query, so a query pasted with a trailing newline finds the same as the bare
//     word, also in the trigram index, where a space is a character).
//
// It is pure and deterministic, safe for concurrent use, never panics on any input, and its
// output is valid UTF-8 without NUL, Cc or Cf runes, variation selectors, two consecutive spaces or
// a leading or trailing space.
//
// What it does not do: accents are not removed (the SQLite tokenizers remove them, on the
// index and on the query alike), and the Cf runes that join within words (U+200C, U+200D, U+2060,
// soft hyphen) are dropped, so "foo<U+2060>bar" holds the one token "foobar".
func NormalizeText(s string) string {
	s = cleanRunes(s, false)
	s = norm.NFKC.String(s)
	s = cases.Fold().String(s) // a fresh Caser per call: it is not safe for concurrent use
	s = norm.NFKC.String(s)
	s = foldExceptions(s)
	s = norm.NFKC.String(s)
	return strings.Trim(cleanRunes(s, true), " ")
}

// cleanRunes applies pipeline step 2: it drops Cf runes and maps Cc, Zs, Zl and Zp runes to U+0020.
// An undecodable byte is written as U+FFFD (the later passes see only valid text). With collapse,
// every run of U+0020 becomes one.
func cleanRunes(s string, collapse bool) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == runeZeroWidthSpace:
			r = ' '
		case unicode.Is(unicode.Cf, r), isVariationSelector(r):
			continue
		case unicode.In(r, unicode.Cc, unicode.Zs, unicode.Zl, unicode.Zp):
			r = ' '
		}
		if r == ' ' {
			if prevSpace && collapse {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// foldExceptions is pipeline step 6. It maps the dotless i to "i" and drops a run of combining
// dots above that follows an "i" (including the one the case folding of U+0130 leaves); the
// dotless i is mapped first so that "dotless i + dot" ends as "i". It also maps the capital
// Cherokee letters (U+13A0-U+13F5) to the small ones (U+AB70-U+ABBF, U+13F8-U+13FD): the case
// folding of x/text maps each Cherokee letter to its OTHER case, so a capital and a small
// letter would come out swapped and never meet (TestNormalizeIsIdempotentOnEveryRune); after
// this step every Cherokee letter is small whichever way the folding went. A single
// left-to-right pass leaves nothing for a second pass to do.
func foldExceptions(s string) string {
	need := false
	for _, r := range s {
		if r == runeDotlessI || r == runeCombiningDot || (r >= 0x13a0 && r <= 0x13f5) {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	afterI := false
	for _, r := range s {
		switch {
		case r == runeDotlessI:
			r = 'i'
		case r == runeCombiningDot && afterI:
			continue
		case r >= 0x13a0 && r <= 0x13ef:
			r += 0xab70 - 0x13a0
		case r >= 0x13f0 && r <= 0x13f5:
			r += 0x13f8 - 0x13f0
		}
		afterI = r == 'i'
		b.WriteRune(r)
	}
	return b.String()
}

var (
	normVersionMu sync.Mutex
	normVersion   string
)

// FTSNormVersion names everything the index content depends on:
// "fts<FTSPipelineVersion>/unicode-<norm.Version>/gounicode-<unicode.Version>/xtext-<module version>/sqlite-<sqlite_version()>". The SQLite part is
// read once from a scratch in-memory database. If that read fails the version carries
// "sqlite-unknown" (and is read again on the next call), which matches no stored index version,
// so a search or an index write is refused with "not current" rather than trusting an unnamed
// build.
func FTSNormVersion() string {
	normVersionMu.Lock()
	defer normVersionMu.Unlock()
	if normVersion != "" {
		return normVersion
	}
	sv, err := sqliteVersion()
	if err != nil {
		return fmt.Sprintf("fts%d/unicode-%s/gounicode-%s/xtext-%s/sqlite-unknown", FTSPipelineVersion, norm.Version, unicode.Version, xTextVersion)
	}
	normVersion = fmt.Sprintf("fts%d/unicode-%s/gounicode-%s/xtext-%s/sqlite-%s", FTSPipelineVersion, norm.Version, unicode.Version, xTextVersion, sv)
	return normVersion
}

func sqliteVersion() (string, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var v string
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

// isVariationSelector reports U+FE00-U+FE0F and U+E0100-U+E01EF. They select a glyph variant of
// the character before them (text or emoji presentation, an ideographic variant) and carry no
// searchable content; platforms differ in whether they append U+FE0F, so keeping them would hide
// the same message from a substring search.
func isVariationSelector(r rune) bool {
	return (r >= 0xfe00 && r <= 0xfe0f) || (r >= 0xe0100 && r <= 0xe01ef)
}
