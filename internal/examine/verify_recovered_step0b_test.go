package examine_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
)

func reproduceProblems(rep evidence.VerifyReport) []string {
	var got []string
	for _, p := range rep.Problems {
		if strings.Contains(p, "reproduce: ") {
			got = append(got, p)
		}
	}
	return got
}

// C59: a cancel after the first chunk ends R6 at the next chunk: it reports cancelled, never a pass, and
// writes nothing but the verify.run entry.
func TestVerifyRecoveredCancelMidRun(t *testing.T) {
	const size = 4 << 20
	c := evidencetest.NewCase(t)
	img := pat(3, size+4096)
	parent := evidencetest.AddImage(t, c, img)
	evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 2048, Length: size}}})
	before := hashTree(t, c.Dir)
	n := len(auditEntries(t, c))
	ctx, cancel := context.WithCancel(context.Background())
	reads := 0
	defer examine.SetReproduceReadObserver(func(int) {
		reads++
		if reads == 1 {
			cancel()
		}
	})()
	rep, err := c.VerifyWith(ctx, examine.RecoveredCheck())
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireProblem(t, rep, "reproduce: verification cancelled")
	if rep.OK() || rep.RecoveredReproduced != 0 {
		t.Errorf("cancelled verify: ok=%v reproduced=%d, want neither", rep.OK(), rep.RecoveredReproduced)
	}
	if reads > 2 {
		t.Errorf("%d reads after the cancel, want the run to stop at the next chunk", reads)
	}
	for name, h := range hashTree(t, c.Dir) {
		if name != "audit.jsonl" && before[name] != h {
			t.Errorf("%s changed", name)
		}
	}
	var added []string
	for _, e := range auditEntries(t, c)[n:] {
		added = append(added, e.Action)
	}
	if !slices.Equal(added, []string{"verify.run"}) {
		t.Errorf("appended %v, want only verify.run", added)
	}
}

// A run outside the image is ONE problem: the read that would follow is not attempted.
func TestVerifyRecoveredOutsideRunIsOneProblem(t *testing.T) {
	const imgSize = 64 << 10
	c, parent, img := r6Image(t, imgSize)
	evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: imgSize - 10, Length: 11}}, Data: make([]byte, 11)})
	if got := reproduceProblems(r6Verify(t, c)); len(got) != 1 {
		t.Errorf("%d reproduce problems %q, want exactly one", len(got), got)
	}
}

// An artifact longer than its runs is not counted as reproduced; one shorter than its runs is compared as
// far as it goes and never reported unreadable.
func TestVerifyRecoveredArtifactLengthAgainstRuns(t *testing.T) {
	c, parent, img := r6Image(t, 256<<10)
	data := slices.Concat(img[4096:4096+8192], img[100000:105000], []byte("extra bytes"))
	evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs, Data: data})
	if rep := r6Verify(t, c); rep.RecoveredReproduced != 0 {
		t.Errorf("an artifact longer than its runs was counted as reproduced (%d)", rep.RecoveredReproduced)
	}
	c2, parent2, img2 := r6Image(t, 256<<10)
	evidencetest.AddRecovered(t, c2, parent2, img2, evidencetest.RecoveredSpec{Runs: r6Runs, Data: img2[4096 : 4096+5000]})
	if got := reproduceProblems(r6Verify(t, c2)); len(got) != 0 {
		t.Errorf("a short artifact gave reproduce problems %q", got)
	}
}

// At most 50 reproduce problems are listed, then one line counts the rest.
func TestVerifyRecoveredProblemCap(t *testing.T) {
	const n = 52
	c, parent, img := r6Image(t, 256<<10)
	for i := range n {
		data := slices.Clone(img[4096 : 4096+512])
		data[0] ^= 1
		evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{
			Runs: []evidence.Run{{Offset: 4096, Length: 512}}, Data: data,
			Path: fmt.Sprintf("recovered/p1-mtfs/%06d-a.bin", i+1),
		})
	}
	got := reproduceProblems(r6Verify(t, c))
	listed, further := 0, 0
	for _, p := range got {
		switch {
		case strings.Contains(p, "differs from image offset"):
			listed++
		case strings.Contains(p, "2 further problems are not listed"):
			further++
		}
	}
	if listed != 50 || further != 1 || len(got) != 51 {
		t.Errorf("%d listed, %d further lines, %d reproduce problems; want 50, 1, 51", listed, further, len(got))
	}
}

// An artifact that ends inside a later chunk of a longer run is compared to its end, not past it.
func TestVerifyRecoveredArtifactEndsMidChunk(t *testing.T) {
	c := evidencetest.NewCase(t)
	img := pat(4, 4<<20)
	parent := evidencetest.AddImage(t, c, img)
	evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{
		Runs: []evidence.Run{{Offset: 0, Length: 3 << 20}}, Data: img[:1<<20+1<<19],
	})
	if got := reproduceProblems(r6Verify(t, c)); len(got) != 0 {
		t.Errorf("reproduce problems %q, want none", got)
	}
}
