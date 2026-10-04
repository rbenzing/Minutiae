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
const FTSPipelineVersion = 1

const (
	runeDotlessI     = 0x0131 // LATIN SMALL LETTER DOTLESS I
	runeCombiningDot = 0x0307 // COMBINING DOT ABOVE
)

// NormalizeText returns the text the full-text indexes hold and queries are compiled from
// (pipeline version 1):
//
//  1. invalid UTF-8 becomes U+FFFD (one per undecodable byte); NUL becomes U+0020;
//  2. every Cf rune (bidi controls, zero-width characters, soft hyphen, BOM) is dropped; every Cc,
//     Zs, Zl and Zp rune becomes U+0020;
//  3. NFKC; 4. full Unicode case folding; 5. NFKC again;
//  6. the deliberate mappings the folding does not give: U+0131 becomes "i", a run of U+0307
//     after an "i" is dropped (the Turkish ones, for recall), and Cherokee letters are mapped to
//     their small forms (the folding of x/text swaps capital and small Cherokee letters, so
//     without this the two spellings of one letter would not meet);
//  7. NFKC again (step 6 can leave a composable pair), then step 2 once more as a guard (so the
//     output contract does not depend on what a Unicode table maps to), and finally every run of
//     U+0020 collapses to one. Leading and trailing spaces are kept (one each at most).
//
// It is pure and deterministic, safe for concurrent use, never panics on any input, and its
// output is valid UTF-8 without NUL, Cc or Cf runes and without two consecutive spaces.
//
// What it does not do: accents are not removed (the SQLite tokenizers remove them, on the
// index and on the query alike), and a Cf rune that separates words in some scripts (U+200B)
// fuses them: a document "foo<U+200B>bar" holds the one token "foobar".
func NormalizeText(s string) string {
	s = cleanRunes(s, false)
	s = norm.NFKC.String(s)
	s = cases.Fold().String(s) // a fresh Caser per call: it is not safe for concurrent use
	s = norm.NFKC.String(s)
	s = foldExceptions(s)
	s = norm.NFKC.String(s)
	return cleanRunes(s, true)
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
		case unicode.Is(unicode.Cf, r):
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
// "fts<FTSPipelineVersion>/unicode-<norm.Version>/sqlite-<sqlite_version()>". The SQLite part is
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
		return fmt.Sprintf("fts%d/unicode-%s/sqlite-unknown", FTSPipelineVersion, norm.Version)
	}
	normVersion = fmt.Sprintf("fts%d/unicode-%s/sqlite-%s", FTSPipelineVersion, norm.Version, sv)
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
