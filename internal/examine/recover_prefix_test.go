package examine_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func recoverAllOpts() examine.RecoverOptions { return examine.RecoverOptions{All: true} }

func TestShortMapIsCappedAt20(t *testing.T) {
	data := pat(3, 4*bs)
	lay := layout(t, []fstest.Node{delNode("/short.bin", data)})
	rm := fstest.RecoverMap{Method: "exfat-nofatchain", Size: 4 * bs, Runs: []filesys.Run{blocks(lay["/short.bin"], 0, 2)}, Assumptions: []string{"stale-chain-stop=loop@7"}}
	e := newRecEnv(t, delNode("/short.bin", data, rm))
	e.recover(t, recoverAllOpts())
	recs := recovered(t, e.c)
	if len(recs) != 1 {
		t.Fatalf("%d artifacts, want 1", len(recs))
	}
	rec := recs[0]
	rv := rec.Source.Derived.Recovery
	if !rec.Incomplete || rec.Size != 2*bs || *rv.Confidence != 20 { // 75 without the cap
		t.Fatalf("incomplete %v size %d confidence %v", rec.Incomplete, rec.Size, *rv.Confidence)
	}
	for _, want := range []string{"prefix-only", fmt.Sprintf("size-declared=%d", 4*bs), "stale-chain-stop=loop@7"} {
		if !slices.Contains(rv.Assumptions, want) {
			t.Errorf("assumptions %v lack %q", rv.Assumptions, want)
		}
	}
	if rv.Alloc.ExcludedBytes != 2*bs || len(rv.Excluded) != 0 {
		t.Errorf("excluded bytes %d runs %v, want %d bytes and no runs", rv.Alloc.ExcludedBytes, rv.Excluded, 2*bs)
	}
	if !strings.Contains(rec.Error, "covers") || !strings.Contains(rec.Error, "stale-chain-stop=loop@7") {
		t.Errorf("error %q lacks the covered-bytes text or the reader's reason", rec.Error)
	}
	if got := readArtifact(t, e.c, rec); string(got) != string(data[:2*bs]) {
		t.Error("artifact differs from the first two blocks")
	}
	verifyOK(t, e.c) // "excluded counters require incomplete" and the reproduction check hold
}

func TestShortMapAndCutTakeTheLowestCap(t *testing.T) {
	data := pat(4, 4*bs)
	lay := layout(t, []fstest.Node{delNode("/a.bin", data)})
	// the map covers 2 of the 4 declared blocks, and its 2nd block is metadata (not free)
	rm := fstest.RecoverMap{Method: mtfsRecM, Size: 4 * bs, Runs: []filesys.Run{blocks(lay["/a.bin"], 0, 1), {Offset: 0, Length: bs}}}
	e := newRecEnv(t, delNode("/a.bin", data, rm))
	e.recover(t, recoverAllOpts())
	recs := recovered(t, e.c)
	if len(recs) != 1 {
		t.Fatalf("%d artifacts", len(recs))
	}
	rv := recs[0].Source.Derived.Recovery
	if *rv.Confidence != 20 || !recs[0].Incomplete || recs[0].Size != bs {
		t.Fatalf("confidence %d incomplete %v size %d, want 20 (never 55) incomplete, %d bytes", *rv.Confidence, recs[0].Incomplete, recs[0].Size, bs)
	}
	// the bytes of the cut run (bs) plus the declared tail the map never named (2*bs)
	if rv.Alloc.ExcludedBytes != 3*bs {
		t.Errorf("ExcludedBytes %d, want %d", rv.Alloc.ExcludedBytes, 3*bs)
	}
	if n := strings.Count(strings.Join(rv.Assumptions, "|"), "prefix-only"); n != 1 {
		t.Errorf("prefix-only appears %d times in %v, want once", n, rv.Assumptions)
	}
	// F74: the cut and the short map both show, in one text (the map's shortness is what the prefix flag adds)
	if !strings.Contains(recs[0].Error, "runs not captured") || !strings.Contains(recs[0].Error, fmt.Sprintf("; the map covers %d of %d declared bytes", 2*bs, 4*bs)) {
		t.Errorf("error %q lacks the cut text and the covered-bytes text", recs[0].Error)
	}
	verifyOK(t, e.c)
}

func TestShortMapWithOverlapStaysAt20(t *testing.T) {
	a, b := pat(5, 4*bs), pat(6, 4*bs)
	lay := layout(t, []fstest.Node{delNode("/a.bin", a), delNode("/b.bin", b)})
	shared := blocks(lay["/a.bin"], 0, 2)
	ma := fstest.RecoverMap{Method: "exfat-nofatchain", Size: 4 * bs, Runs: []filesys.Run{shared}}
	mb := fstest.RecoverMap{Method: "exfat-nofatchain", Size: 3 * bs, Runs: []filesys.Run{shared}}
	e := newRecEnv(t, delNode("/a.bin", a, ma), delNode("/b.bin", b, mb))
	e.recover(t, recoverAllOpts())
	recs := recovered(t, e.c)
	if len(recs) != 2 {
		t.Fatalf("%d artifacts, want 2", len(recs))
	}
	for _, rec := range recs {
		rv := rec.Source.Derived.Recovery
		if *rv.Confidence != 20 {
			t.Errorf("%s confidence %d, want 20", rec.Path, *rv.Confidence)
		}
		found := false
		for _, as := range rv.Assumptions {
			found = found || strings.HasPrefix(as, "overlap=")
		}
		if !found {
			t.Errorf("%s assumptions %v name no overlap", rec.Path, rv.Assumptions)
		}
	}
}

func TestNoRunsSkipNamesTheReaderStop(t *testing.T) {
	data := pat(7, 2*bs)
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.recoverable = func(inner filesys.Recoverer, en filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := inner.Recoverable(en)
			for i := range cs {
				cs[i].Runs = nil
				cs[i].Size = 2 * bs
				cs[i].Assumptions = []string{"stale-chain-stop=free-entry@5"}
			}
			return cs, err
		}
	}, delNode("/x.bin", data))
	sum := e.recover(t, recoverAllOpts())
	if sum.SkippedBy["no-runs"] != 1 {
		t.Fatalf("SkippedBy %v, want one no-runs", sum.SkippedBy)
	}
	found := false
	for _, w := range auditByAction(t, e.c, "analysis.warning") {
		if r, _ := w.Details["reason"].(string); strings.Contains(r, "no-runs") && strings.Contains(r, "stale-chain-stop=free-entry@5") {
			found = true
		}
	}
	if !found {
		t.Errorf("no audited no-runs warning names the reader's stop: %v", auditByAction(t, e.c, "analysis.warning"))
	}
}
