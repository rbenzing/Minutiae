package examine_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
)

func poolPath(i int) string { return fmt.Sprintf("recovered/p1-mtfs/%06d-a.bin", i) }

// poolArtifact adds recovered artifact i of parent: 700 bytes at offset i*1000, with one byte flipped at the
// first, middle or last position when tamper is 1, 2 or 3, and a run outside the image when tamper is 4.
func poolArtifact(t *testing.T, c *evidence.Case, parent evidence.ManifestRecord, img []byte, i, tamper int) evidence.ManifestRecord {
	t.Helper()
	spec := evidencetest.RecoveredSpec{Path: poolPath(i), Runs: []evidence.Run{{Offset: int64(i) * 1000, Length: 700}}}
	data := append([]byte(nil), img[i*1000:i*1000+700]...)
	switch tamper {
	case 1:
		data[0] ^= 1
	case 2:
		data[350] ^= 1
	case 3:
		data[699] ^= 1
	case 4:
		spec.Runs = []evidence.Run{{Offset: int64(len(img)) - 10, Length: 700}}
		data = make([]byte, 700)
	}
	spec.Data = data
	return evidencetest.AddRecovered(t, c, parent, img, spec)
}

func TestReproduceParallelEqualsSerial(t *testing.T) {
	c := evidencetest.NewCase(t)
	img1, img2 := pat(7, 128<<10), pat(9, 128<<10)
	p1 := evidencetest.AddImage(t, c, img1)
	recs, _ := importFiles(t, c, []string{"second.img"}, [][]byte{img2})
	p2 := recs[0]
	for i := 0; i < 40; i++ {
		tamper := 0
		switch {
		case i < 28 && i%4 == 1: // 1, 5, 9, 13, 17, 21, 25: seven tampered artifacts
			tamper = 1 + (i/4)%3
		case i >= 36 && i < 39:
			tamper = 4
		}
		poolArtifact(t, c, p1, img1, i, tamper)
	}
	for i := 0; i < 60; i++ {
		poolArtifact(t, c, p2, img2, i, 1+i%3)
	}
	run := func(workers int) evidence.VerifyReport {
		t.Helper()
		defer examine.SetReproduceWorkers(workers)()
		rep, err := c.VerifyWith(context.Background(), examine.RecoveredCheck())
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	one, eight := run(1), run(8)
	if !reflect.DeepEqual(one.Problems, eight.Problems) || !reflect.DeepEqual(one.Notices, eight.Notices) || one.RecoveredReproduced != eight.RecoveredReproduced {
		t.Fatalf("workers 1 and 8 differ:\n%q\n%q\nreproduced %d vs %d", one.Problems, eight.Problems, one.RecoveredReproduced, eight.RecoveredReproduced)
	}
	var listed, descents int
	var prev string
	for _, p := range one.Problems {
		if strings.HasPrefix(p, "reproduce: ") {
			listed++
			if i := strings.LastIndex(p, ", \""); i >= 0 {
				path := p[i:]
				if path < prev {
					descents++ // one restart is expected, where the second parent begins
				}
				prev = path
			}
		}
	}
	if descents > 1 { // at most one restart, where the second parent begins (which parent sorts first depends on the ids)
		t.Errorf("%d descents in the order of the listed problems, want at most 1 (parent id, then path)", descents)
	}
	if listed != 51 { // 50 listed, then the further-problems line
		t.Errorf("%d reproduce problems, want 50 and one further-problems line", listed)
	}
	// 7 tampered and 3 out-of-image artifacts in the first parent plus 60 in the second: 70 findings, 20 beyond the cap
	evidencetest.RequireProblem(t, one, "reproduce: 20 further problems are not listed")
	if one.RecoveredReproduced != 30 {
		t.Errorf("reproduced = %d, want the 30 clean artifacts of the first parent", one.RecoveredReproduced)
	}
}

func TestReproduceRunsInParallel(t *testing.T) {
	c := evidencetest.NewCase(t)
	img := pat(7, 64<<10)
	p := evidencetest.AddImage(t, c, img)
	for i := 0; i < 8; i++ {
		poolArtifact(t, c, p, img, i, 0)
	}
	defer examine.SetReproduceWorkers(4)()
	var inside, peak atomic.Int32
	defer examine.SetReproduceOneHook(func(string) {
		n := inside.Add(1)
		defer inside.Add(-1)
		for {
			if old := peak.Load(); n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		// wait (bounded) for a second artifact to be inside at the same moment
		for deadline := time.Now().Add(3 * time.Second); inside.Load() < 2 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
	})()
	rep, err := c.VerifyWith(context.Background(), examine.RecoveredCheck())
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() < 2 {
		t.Errorf("at most %d artifacts were reproduced at once with 4 workers, want at least 2", peak.Load())
	}
	if rep.RecoveredReproduced != 8 {
		t.Errorf("reproduced = %d, want 8 (problems %q)", rep.RecoveredReproduced, rep.Problems)
	}
}

// A cancel right after the tampered artifact's mismatching chunk was compared keeps that finding: exit 4, not 1.
func TestReproduceCancelKeepsFindings(t *testing.T) {
	c := evidencetest.NewCase(t)
	img := pat(7, 64<<10)
	p := evidencetest.AddImage(t, c, img)
	for i := 0; i < 6; i++ {
		poolArtifact(t, c, p, img, i, 0)
	}
	// the tampered artifact is the only one of 100 bytes: its image read and its artifact read are the two reads of that size
	bad := evidencetest.AddRecovered(t, c, p, img, evidencetest.RecoveredSpec{
		Path: poolPath(9), Runs: []evidence.Run{{Offset: 20000, Length: 100}}, Data: append([]byte{^img[20000]}, img[20001:20100]...),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reads atomic.Int32
	defer examine.SetReproduceReadObserver(func(n int) {
		if n == 100 && reads.Add(1) == 2 {
			cancel()
		}
	})()
	defer examine.SetReproduceWorkers(4)()
	rep, err := c.VerifyWith(ctx, examine.RecoveredCheck())
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("reproduce: byte 0 of the artifact differs from image offset 20000 (artifact %q", bad.ID))
	evidencetest.RequireProblem(t, rep, "reproduce: verification cancelled")
	if rep.CancelledOnly() {
		t.Error("a cancelled verify that holds a finding must not be cancel-only (exit 4, never 1)")
	}
}

func TestReproducePanicIsAProblem(t *testing.T) {
	c := evidencetest.NewCase(t)
	img := pat(7, 64<<10)
	p := evidencetest.AddImage(t, c, img)
	var victim evidence.ManifestRecord
	for i := 0; i < 5; i++ {
		r := poolArtifact(t, c, p, img, i, 0)
		if i == 2 {
			victim = r
		}
	}
	defer examine.SetReproduceWorkers(4)()
	defer examine.SetReproduceOneHook(func(id string) {
		if id == victim.ID {
			panic("boom")
		}
	})()
	rep, err := c.VerifyWith(context.Background(), examine.RecoveredCheck())
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireProblem(t, rep, "reproduce: check panicked: boom")
	if rep.RecoveredReproduced != 4 {
		t.Errorf("reproduced = %d, want the 4 others (problems %q)", rep.RecoveredReproduced, rep.Problems)
	}
}

func TestReproduceFeedsProgress(t *testing.T) {
	c := evidencetest.NewCase(t)
	img := pat(7, 64<<10)
	p := evidencetest.AddImage(t, c, img)
	for i := 0; i < 5; i++ {
		poolArtifact(t, c, p, img, i, 0)
	}
	prog := evidence.NewVerifyProgress()
	var mu sync.Mutex
	var snaps []evidence.VerifyProgressSnapshot
	defer examine.SetReproduceReadObserver(func(int) {
		mu.Lock()
		snaps = append(snaps, prog.Snapshot())
		mu.Unlock()
	})()
	defer examine.SetReproduceWorkers(1)()
	rep, err := c.VerifyObserved(context.Background(), prog, examine.VerifyChecks(examine.Options{})...)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RecoveredReproduced != 5 {
		t.Fatalf("reproduced = %d (problems %q)", rep.RecoveredReproduced, rep.Problems)
	}
	if len(snaps) == 0 {
		t.Fatal("no reads observed")
	}
	var last int64
	for i, s := range snaps {
		if s.Phase != "checks/reproduce" || !s.Counted || s.Unit != "bytes" || s.Total != 5*700 {
			t.Fatalf("snapshot %d = %+v, want counted checks/reproduce in bytes of total 3500", i, s)
		}
		if s.Done < last || s.Done > s.Total {
			t.Fatalf("snapshot %d done %d after %d (total %d)", i, s.Done, last, s.Total)
		}
		last = s.Done
	}
	if last == 0 {
		t.Error("the byte counter never moved")
	}
	if s := prog.Snapshot(); s.Phase != "" {
		t.Errorf("phase %q is left running", s.Phase)
	}
}

func TestVerifyChecksStartsWithReproduce(t *testing.T) {
	if len(examine.VerifyChecks(examine.Options{})) < 1 {
		t.Fatal("no composed checks")
	}
	c := evidencetest.NewCase(t)
	img := pat(7, 64<<10)
	poolArtifact(t, c, evidencetest.AddImage(t, c, img), img, 0, 0)
	rep, err := c.VerifyWith(context.Background(), examine.VerifyChecks(examine.Options{})...)
	if err != nil {
		t.Fatal(err)
	}
	// the fake parent is a plain image with no filesystem: R9 cannot re-open it and says so (never a silent pass)
	evidencetest.RequireProblem(t, rep, "allocation: partition 1 of parent")
	if rep.RecoveredReproduced != 1 {
		t.Errorf("reproduced = %d, want 1 (problems %q)", rep.RecoveredReproduced, rep.Problems)
	}
}
