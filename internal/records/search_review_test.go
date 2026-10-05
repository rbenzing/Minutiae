package records_test

import (
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestSearchSnippetsOnlyTheSearchedColumn (R51): the snippet of a column the query did not search
// is not built, so nothing of it is highlighted or shown as a hit.
func TestSearchSnippetsOnlyTheSearchedColumn(t *testing.T) {
	c, art := setup(t)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, []records.Record{
		{Type: "note", ArtifactID: art.ID, Summary: "needle in summary", Body: "needle in body", Payload: map[string]any{}},
	})
	r := newReader(t, c)
	for _, tc := range []struct {
		col                   records.Column
		wantSummary, wantBody bool
	}{
		{records.ColAny, true, true},
		{records.ColSummary, true, false},
		{records.ColBody, false, true},
	} {
		q := mustCompile(t, "needle", records.TextOptions{Column: tc.col})
		res, err := r.Search(ctx, records.Filter{Text: q}, records.Page{Limit: 10}, records.SearchOptions{Snippets: true})
		if err != nil || len(res.Hits) != 1 {
			t.Fatalf("column %d: %v, %d hits", tc.col, err, len(res.Hits))
		}
		h := res.Hits[0]
		if (h.Summary != nil) != tc.wantSummary || (h.Body != nil) != tc.wantBody {
			t.Errorf("column %d: summary snippet %v, body snippet %v; want %v and %v", tc.col, h.Summary != nil, h.Body != nil, tc.wantSummary, tc.wantBody)
		}
	}
}

// TestSearchRefusesOptionsItWouldIgnore (R52): a snippet width without snippets, and a rank query
// on List, Count, Stats or Overview, are ErrInvalidPage, never silently ignored.
func TestSearchRefusesOptionsItWouldIgnore(t *testing.T) {
	fx := newTextFix(t)
	plain := records.Filter{Text: mustCompile(t, "cafe", records.TextOptions{})}
	if _, err := fx.r.Search(ctx, plain, records.Page{}, records.SearchOptions{SnippetWidth: 50}); !errors.Is(err, records.ErrInvalidPage) {
		t.Errorf("width without snippets: %v, want ErrInvalidPage", err)
	}
	ranked := records.Filter{Text: mustCompile(t, "cafe", records.TextOptions{Rank: true})}
	if _, err := fx.r.List(ctx, ranked, records.Page{}); !errors.Is(err, records.ErrInvalidPage) {
		t.Errorf("List rank: %v", err)
	}
	if _, _, err := fx.r.Count(ctx, ranked, 0); !errors.Is(err, records.ErrInvalidPage) {
		t.Errorf("Count rank: %v", err)
	}
	if _, err := fx.r.Stats(ctx, ranked, "type"); !errors.Is(err, records.ErrInvalidPage) {
		t.Errorf("Stats rank: %v", err)
	}
	if _, err := fx.r.Overview(ctx, ranked); !errors.Is(err, records.ErrInvalidPage) {
		t.Errorf("Overview rank: %v", err)
	}
}

// TestSearchLeavesTheCaseUntouched (R54): Search and its snippets change no file of the case but
// audit.jsonl, and the Reader code that runs them has no write path of its own (every statement goes
// through the ReadHandle, which refuses anything but a read).
func TestSearchLeavesTheCaseUntouched(t *testing.T) {
	fx := newTextFix(t)
	snap := func() map[string][32]byte {
		out := map[string][32]byte{}
		err := filepath.WalkDir(fx.c.Dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || filepath.Base(p) == "audit.jsonl" || filepath.Base(p) == "case.lock" {
				return err
			}
			b, rerr := os.ReadFile(p) //nolint:gosec // a test reading the files of its own temp case
			if rerr != nil {
				return rerr
			}
			out[p] = sha256.Sum256(b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snap()
	for _, o := range []records.TextOptions{{}, {Substring: true}, {Rank: true}} {
		q := mustCompile(t, "cafe", o)
		if _, err := fx.r.Search(ctx, records.Filter{Text: q}, records.Page{Limit: 50}, records.SearchOptions{Snippets: !o.Rank}); err != nil {
			t.Fatal(err)
		}
	}
	after := snap()
	if len(before) != len(after) {
		t.Fatalf("files before %d, after %d", len(before), len(after))
	}
	for p, h := range before {
		if after[p] != h {
			t.Errorf("Search changed %s", p)
		}
	}
	for _, f := range []string{"search.go", "snippet.go", "termhits.go", "list.go", "stats.go", "reader.go", "get.go", "indexstatus.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"StoreTx(", "ExecContext(", ".Exec(", "BeginTx(", ".Begin("} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s uses %s: a read path must go through the ReadHandle only", f, bad)
			}
		}
	}
}

// TestSnippetFoldsSupplementaryLettersAndMarksOnManyBases (R55): the display-time fold agrees with
// the real tokenizers for letters outside the BMP (the mathematical alphanumerics NFKC maps to plain
// letters, and a stride through every other supplementary letter) and for every combining mark on
// bases of several scripts, not only on "a".
func TestSnippetFoldsSupplementaryLettersAndMarksOnManyBases(t *testing.T) {
	fx := newQueryFixture(t)
	word := foldProbe{t, fx, evidence.FTSWordTable}
	sub := foldProbe{t, fx, evidence.FTSSubTable}
	stripped := map[string]int{}
	for _, base := range []string{"e", "o", "n", "u", "s", "z", "i", cp(0x430), cp(0x3b1), cp(0x5d0)} {
		plain := "x" + base + base + "x"
		for r := rune(0x80); r <= unicode.MaxRune; r++ {
			if !unicode.Is(unicode.M, r) {
				continue
			}
			doc := "x" + base + string(r) + base + "x"
			found := word.finds(doc, plain)
			if found {
				stripped[base]++
			}
			if got := highlights(t, doc, plain, records.TextOptions{}); got != found {
				t.Errorf("base %q U+%04X: the word index finds %v, the snippet highlights %v", base, r, found, got)
			}
			if got := highlights(t, doc, plain, records.TextOptions{Substring: true}); got != sub.finds(doc, plain) {
				t.Errorf("base %q U+%04X: the substring snippet and the trigram index disagree", base, r)
			}
		}
	}
	for _, base := range []string{"e", "o", "n", "u", "s", "z", "i"} {
		if stripped[base] == 0 {
			t.Errorf("no mark is stripped on %q: the sweep is vacuous", base)
		}
	}
	checked := 0
	for r := rune(0x10000); r <= unicode.MaxRune; r++ {
		if !unicode.IsLetter(r) {
			continue
		}
		mathAlnum := r >= 0x1D400 && r <= 0x1D7FF
		if !mathAlnum && r%23 != 0 {
			continue
		}
		nt := evidence.NormalizeText(string(r))
		if nt == "" || strings.ContainsAny(nt, "\"") {
			continue
		}
		doc, query := "x"+string(r)+"x", "x"+nt+"x"
		for name, p := range map[string]foldProbe{"word": word, "trigram": sub} {
			found := p.finds(doc, query)
			o := records.TextOptions{Substring: name == "trigram"}
			if got := highlights(t, doc, query, o); got != found {
				t.Errorf("U+%05X (%s): the index finds %v, the snippet highlights %v", r, name, found, got)
			}
		}
		checked++
	}
	if checked < 500 {
		t.Fatalf("only %d supplementary letters swept", checked)
	}
}
