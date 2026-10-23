package records_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// cjkCase ingests one record per summary (ids 1..n) and returns the case and a reader.
func cjkCase(t *testing.T, summaries ...string) (*evidence.Case, *records.Reader) {
	t.Helper()
	c, art := setup(t)
	recs := make([]records.Record, len(summaries))
	for i, s := range summaries {
		recs[i] = records.Record{Type: "note", ArtifactID: art.ID, Summary: s, Payload: map[string]any{"i": i}}
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	return c, newReader(t, c)
}

func searchIDs(t *testing.T, r *records.Reader, in string, o records.TextOptions) []int64 {
	t.Helper()
	q, err := records.CompileQuery(in, o)
	if err != nil {
		return nil
	}
	res, err := r.Search(ctx, records.Filter{Text: q}, records.Page{Limit: records.MaxLimit}, records.SearchOptions{})
	if err != nil {
		t.Fatalf("Search(%q): %v", in, err)
	}
	ids := hitIDs(res.Hits)
	slices.Sort(ids)
	return ids
}

// TestSearchFindsCJKWordsInsideARun (Task 4, L86): every CJK letter is a token, so a word is found
// inside a run, also a one-letter word, and a 2-letter piece by substring.
func TestSearchFindsCJKWordsInsideARun(t *testing.T) {
	_, r := cjkCase(t,
		cp(0x65e5, 0x672c, 0x8a9e, 0x306e, 0x30c6, 0x30ad, 0x30b9, 0x30c8), // 1
		cp(0x6771, 0x4eac, 0x90fd, 0x5e81),                                 // 2
		cp(0x4eac, 0x90fd),                                                 // 3
		cp(0x6771, ' ', 0x4eac),                                            // 4
	)
	for _, c := range []struct {
		name string
		in   string
		o    records.TextOptions
		want []int64
	}{
		{"two letters at the start", cp(0x65e5, 0x672c), records.TextOptions{}, []int64{1}},
		{"two letters in the middle", cp(0x672c, 0x8a9e), records.TextOptions{}, []int64{1}},
		{"a word in two records", cp(0x4eac, 0x90fd), records.TextOptions{}, []int64{2, 3}},
		{"one letter", cp(0x90fd), records.TextOptions{}, []int64{2, 3}},
		{"a word across a particle", cp(0x8a9e, 0x306e), records.TextOptions{}, []int64{1}},
		{"a space and no space meet (M8)", cp(0x6771, 0x4eac), records.TextOptions{}, []int64{2, 4}},
		{"substring of two letters (M8)", cp(0x6771, 0x4eac), records.TextOptions{Substring: true}, []int64{2, 4}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := searchIDs(t, r, c.in, c.o); !slices.Equal(got, c.want) {
				t.Errorf("%q: got %v, want %v", c.in, got, c.want)
			}
		})
	}
	if _, err := records.CompileQuery(cp(0x4eac), records.TextOptions{Substring: true}); !errors.Is(err, records.ErrInvalidQuery) {
		t.Errorf("a one-letter substring: %v, want ErrInvalidQuery", err)
	}
}

// TestOldPipelineIndexIsNotCurrentAfterTheBump (M22, Review Focus 2): an index built by pipeline 3 is
// never searched by pipeline 4. EVERY text query on it, an ASCII word included, is "not current" with
// the `records reindex` remedy; after the reindex each finds its record.
func TestOldPipelineIndexIsNotCurrentAfterTheBump(t *testing.T) {
	c, r := cjkCase(t, cp(0x65e5, 0x672c, 0x8a9e, 0x306e, 0x30c6, 0x30ad, 0x30b9, 0x30c8), "hello world")
	old := strings.Replace(evidence.FTSNormVersion(), "fts4/", "fts3/", 1)
	if old == evidence.FTSNormVersion() {
		t.Fatalf("FTSNormVersion() = %q is not a pipeline 4 version", old)
	}
	recordstest.SetFTSNormVersion(t, c.Dir, old)
	queries := []struct {
		name string
		in   string
		o    records.TextOptions
		want int64
	}{
		{"CJK word", cp(0x672c, 0x8a9e), records.TextOptions{}, 1},
		{"ASCII word", "hello", records.TextOptions{}, 2},
		{"prefix", "hel*", records.TextOptions{}, 2},
		{"substring", "ell", records.TextOptions{Substring: true}, 2},
	}
	for _, q := range queries {
		cq := mustCompile(t, q.in, q.o)
		_, err := r.Search(ctx, records.Filter{Text: cq}, records.Page{Limit: 10}, records.SearchOptions{})
		if !errors.Is(err, evidence.ErrIndexNotCurrent) || !strings.Contains(err.Error(), "records reindex") {
			t.Errorf("%s on the old index: %v, want ErrIndexNotCurrent naming `records reindex`", q.name, err)
		}
	}
	if _, err := c.ReindexText(ctx, evidence.ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, q := range queries {
		if got := searchIDs(t, r, q.in, q.o); !slices.Equal(got, []int64{q.want}) {
			t.Errorf("%s after the reindex: %v, want [%d]", q.name, got, q.want)
		}
	}
}

// TestVerifyCleanOnSegmentedCJK (Review Focus 4): the segmented text, mixed with Latin and with a
// combining mark after kana, is what the index holds and what P11 verify recomputes: no problem.
// It checks that the index and P11 agree; it is not a segmentation oracle (TestSearchFindsCJKWordsInsideARun is).
func TestVerifyCleanOnSegmentedCJK(t *testing.T) {
	c, art := setup(t)
	texts := []string{
		cp(0x65e5, 0x672c, 0x8a9e, 0x306e, 0x30c6, 0x30ad, 0x30b9, 0x30c8),
		cp(0x6771, 0x4eac) + "tower " + cp(0x3068, ' ', 0x4eac, 0x90fd),
		cp(0x30a2, 0x309a, 0x30a4, ' ', 0x30ac),
		cp(0x3300, ' ', 0x4eba, 0x3005),
	}
	var recs []records.Record
	indexable := 0
	for i, s := range texts {
		body := s
		if _, ok := evidence.NewFTSDoc(int64(i+1), s, &body); ok {
			indexable++
		}
		recs = append(recs, records.Record{Type: "note", ArtifactID: art.ID, Summary: s, Body: body, Payload: map[string]any{"i": i}})
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	rep, err := c.Verify()
	if err != nil || !rep.OK() {
		t.Fatalf("verify: %v %q", err, rep.Problems)
	}
	if rep.FTSDocsChecked != indexable || indexable != len(texts) {
		t.Fatalf("FTSDocsChecked = %d, want the %d indexable records", rep.FTSDocsChecked, indexable)
	}
}

// TestCJKPrefixCountsTheRun (M10, L86): a star after a CJK letter counts the run of CJK letters
// before it, so `東京*` is accepted and `京*` alone is not.
func TestCJKPrefixCountsTheRun(t *testing.T) {
	q, err := records.CompileQuery(cp(0x6771, 0x4eac, '*'), records.TextOptions{})
	if err != nil {
		t.Fatalf("a two-letter CJK prefix: %v", err)
	}
	if want := `("` + cp(0x6771, ' ', 0x4eac) + `" *)`; q.Match() != want {
		t.Errorf("Match() = %q, want %q", q.Match(), want)
	}
	if _, err := records.CompileQuery(cp(0x4eac, '*'), records.TextOptions{}); !errors.Is(err, records.ErrInvalidQuery) {
		t.Errorf("a one-letter CJK prefix: %v, want ErrInvalidQuery", err)
	}
	if _, err := records.CompileQuery("ab"+cp(0x4eac, '*'), records.TextOptions{}); err != nil {
		t.Errorf("ab + one CJK letter and a star: %v", err)
	}
	if _, err := records.CompileQuery(cp(0x6771, 0x4eac, 0x90fd, '*'), records.TextOptions{}); err != nil {
		t.Errorf("a three-letter CJK prefix: %v", err)
	}
	_, r := cjkCase(t,
		cp(0x6771, 0x4eac, 0x90fd, 0x5e81), // 1
		cp(0x6771, 0x4eac)+"tower",         // 2
		cp(0x4eac, 0x90fd),                 // 3
	)
	if got := searchIDs(t, r, cp(0x6771, 0x4eac, '*'), records.TextOptions{}); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("search of the prefix: %v, want [1 2]", got)
	}
}

// TestCJKPrefixStepsOverMarks (LX6-T5-1): a combining mark kept after a CJK letter does not break
// the run, so U+20DD between two letters still counts both, and a lone marked letter is refused.
func TestCJKPrefixStepsOverMarks(t *testing.T) {
	if _, err := records.CompileQuery(cp(0x6771, 0x20dd, 0x4eac, '*'), records.TextOptions{}); err != nil {
		t.Errorf("two CJK letters with a mark between them: %v", err)
	}
	if _, err := records.CompileQuery(cp(0x4eac, 0x20dd, '*'), records.TextOptions{}); !errors.Is(err, records.ErrInvalidQuery) {
		t.Errorf("one marked CJK letter and a star: %v, want ErrInvalidQuery", err)
	}
}
