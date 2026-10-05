package records_test

import (
	"strings"
	"testing"
	"unicode"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

// foldProbe indexes one document through the same path the writer uses and reports whether the
// full-text table finds the phrase.
type foldProbe struct {
	t     *testing.T
	fx    *queryFixture
	table string
}

func (p foldProbe) finds(doc, phrase string) bool {
	p.t.Helper()
	d, ok := evidence.NewFTSDoc(5000, doc, nil)
	if !ok {
		return false
	}
	if _, err := p.fx.db.Exec(`INSERT INTO `+p.table+`(rowid, summary, body) VALUES (?, ?, ?)`, d.ID, d.Summary, d.Body); err != nil { //nolint:gosec // one of the two FTS table constants
		p.t.Fatal(err)
	}
	defer func() {
		if _, err := p.fx.db.Exec(`INSERT INTO `+p.table+`(`+p.table+`, rowid, summary, body) VALUES ('delete', ?, ?, ?)`, d.ID, d.Summary, d.Body); err != nil { //nolint:gosec // one of the two FTS table constants
			p.t.Fatal(err)
		}
	}()
	ids, err := p.fx.match(p.table, `"`+phrase+`"`)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, id := range ids {
		if id == d.ID {
			return true
		}
	}
	return false
}

// highlights reports whether the snippet of doc for the query marks anything.
func highlights(t *testing.T, doc, query string, o records.TextOptions) bool {
	t.Helper()
	q, err := records.CompileQuery(query, o)
	if err != nil {
		t.Fatal(err)
	}
	s := records.SnippetFor(doc, q, 120)
	return s != nil && len(snippetMatches(s)) > 0
}

// TestSnippetFoldsLikeTheIndex (R49): the display-time comparison folds exactly as the index does:
// for every combining mark of Unicode, a text in which the real tokenizer drops the mark (so the
// plain word finds it) is highlighted by the plain word, and a text in which the tokenizer keeps the
// mark (the plain word does not find it) is not; both tokenizers (word and trigram) and the
// precomposed Latin letters are swept.
func TestSnippetFoldsLikeTheIndex(t *testing.T) {
	fx := newQueryFixture(t)
	word := foldProbe{t, fx, evidence.FTSWordTable}
	sub := foldProbe{t, fx, evidence.FTSSubTable}
	stripped := 0
	for r := rune(0x80); r <= unicode.MaxRune; r++ {
		if !unicode.Is(unicode.M, r) {
			continue
		}
		doc := "xaa" + string(r) + "aa"
		found := word.finds(doc, "xaaaa")
		if found {
			stripped++
		}
		if got := highlights(t, doc, "xaaaa", records.TextOptions{}); got != found {
			t.Errorf("U+%04X: the word index finds %v, the snippet highlights %v", r, found, got)
		}
		if subFound := sub.finds(doc, "xaaaa"); subFound != found {
			t.Errorf("U+%04X: the trigram index finds %v but the word index %v: one fold rule no longer serves both", r, subFound, found)
		}
		if got := highlights(t, doc, "xaaaa", records.TextOptions{Substring: true}); got != found {
			t.Errorf("U+%04X: the substring snippet highlights %v, the index finds %v", r, got, found)
		}
	}
	if stripped == 0 {
		t.Fatal("the tokenizer strips no combining mark: the sweep is vacuous")
	}
	// letters the fold changes (precomposed ones with a stripped accent): the real tokenizer reduces them
	// to the folded form exactly when the snippet does
	reduced := 0
	for r := rune(0x80); r <= 0xFFFF; r++ {
		if !unicode.IsLetter(r) {
			continue
		}
		nt := evidence.NormalizeText(string(r))
		folded := evidence.FTSFoldDiacritics(nt)
		if nt == "" || strings.ContainsAny(folded, "\"") {
			continue
		}
		if folded == nt {
			// the fold leaves it alone: in the Latin blocks the tokenizer must not reduce it to a plain
			// letter either (a letter missing from the table would lose its highlight)
			if rs := []rune(nt); len(rs) == 1 && rs[0] >= 0x80 && (r <= 0x24F || r >= 0x1E00 && r <= 0x1EFF || r >= 0x2C60 && r <= 0x2C7F || r >= 0xA720 && r <= 0xA7FF) {
				for a := 'a'; a <= 'z'; a++ {
					if word.finds("x"+string(r)+"x", "x"+string(a)+"x") {
						t.Errorf("U+%04X: the word index reduces it to %q but the fold keeps it", r, a)
					}
				}
			}
			continue
		}
		doc, plain := "x"+string(r)+"x", "x"+folded+"x"
		found := word.finds(doc, plain)
		if got := highlights(t, doc, plain, records.TextOptions{}); got != found {
			t.Errorf("U+%04X (folds to %q): the word index finds %v, the snippet highlights %v", r, folded, found, got)
		}
		reduced++
	}
	if reduced < 100 {
		t.Fatalf("only %d letters change under the fold: the sweep is vacuous", reduced)
	}
	// the shared rule and the normalization together: the mark of the NFD form is one match
	if got := evidence.FTSFoldDiacritics("cafe" + cp(0x301) + cp(0xe9)); got != "cafee" {
		t.Errorf("FTSFoldDiacritics(cafe + acute + precomposed e acute) = %q, want cafee", got)
	}
}
