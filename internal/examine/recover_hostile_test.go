package examine_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func TestRecoverSkipsDuplicateEntryIDs(t *testing.T) {
	base := fstest.Build(fstest.BuildSpec{Label: "L", BlockSize: int(buildBS), FreeBlocks: 4, Nodes: []fstest.Node{
		delNode("/dup-a.bin", pat(1, bs)), delNode("/dup-b.bin", pat(2, bs)), delNode("/ok.bin", pat(3, bs)),
	}})
	// Hostile image: the second record takes the id of the first (same length, so every offset holds).
	tableLen := int(binary.LittleEndian.Uint64(base[8:16]))
	table := bytes.Replace(base[16:16+tableLen], []byte(`"id":"3","parent_id":"1","name":"dup-b.bin"`), []byte(`"id":"2","parent_id":"1","name":"dup-b.bin"`), 1)
	if bytes.Equal(table, base[16:16+tableLen]) {
		t.Fatal("the table layout changed: the id of dup-b.bin was not rewritten")
	}
	hostile := fstest.Encode(table, base[16+tableLen:])
	c := newCase(t)
	recs := importImage(t, c, disk(hostile), 1)
	s := openSession(t, c, recs[0].ID)
	sum, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.SkippedBy["duplicate-id"] != 2 {
		t.Fatalf("summary = %+v, want both entries skipped duplicate-id", sum)
	}
	for _, r := range recovered(t, c) {
		if strings.Contains(r.Path, "dup-") || strings.HasPrefix(r.Source.Derived.FSPath, "/dup-") {
			t.Errorf("duplicate-id entry named in artifact %q", r.Path)
		}
	}
	var warned bool
	for _, r := range warnReasons(t, c) {
		if strings.Contains(r, "duplicate-id") && strings.Contains(r, "/dup-a.bin") && strings.Contains(r, "/dup-b.bin") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("warnings = %q, want one naming both paths", warnReasons(t, c))
	}
}

func TestRecoverNeverAsksForALiveEntry(t *testing.T) {
	listed := map[string]bool{} // ids ReadDir listed as deleted
	var asked []filesys.Entry
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.onReadDir = func(_ filesys.Entry, kids []filesys.Entry) {
			for _, k := range kids {
				if k.Deleted {
					listed[k.ID] = true
				}
			}
		}
		h.recoverable = func(in filesys.Recoverer, en filesys.Entry) ([]filesys.Candidate, error) {
			asked = append(asked, en)
			return in.Recoverable(en)
		}
	}, delNode("/gone.bin", pat(1, bs)), fstest.Node{Path: "/live.bin", Data: pat(2, bs)}, fstest.Node{Path: "/d", Dir: true})
	e.recover(t, examine.RecoverOptions{All: true})
	if len(asked) == 0 {
		t.Fatal("Recoverable was never called")
	}
	for _, en := range asked {
		if !listed[en.ID] || !en.Deleted {
			t.Errorf("Recoverable asked for %+v, which ReadDir did not list as deleted", en)
		}
	}
	asked = nil
	_, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, Refs: []string{"id:" + idOf2(t, e, "/live.bin")}})
	if !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("live id ref: %v, want ErrNotDeleted", err)
	}
	if len(asked) != 0 {
		t.Errorf("Recoverable asked for a live entry: %+v", asked)
	}
}

func TestRecoverTrustsNoReaderOutput(t *testing.T) {
	data := pat(1, 2*bs)
	nodes := []fstest.Node{delNode("/a.bin", data), {Path: "/live.bin", Data: pat(2, bs)}}
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.recoverable = func(in filesys.Recoverer, en filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := in.Recoverable(en)
			for i := range cs {
				cs[i].Mode = 0o7777
				cs[i].Encrypted = true
				cs[i].Times.Modified = filesys.Timestamp{T: cs[i].Times.Modified.T.AddDate(5000, 0, 0), ZoneKnown: true}
			}
			// one more candidate that claims 1 TiB and runs beyond the partition
			cs = append(cs, filesys.Candidate{Method: mtfsRecM, Size: 1 << 40, Runs: []filesys.Run{{Offset: 0, Length: 1 << 40}}})
			return cs, err
		}
	}, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true, AllCandidates: true})
	recs := recovered(t, e.c)
	if len(recs) != 1 {
		t.Fatalf("%d artifacts, want 1", len(recs))
	}
	if sum.SkippedBy["invalid-map"] != 1 {
		t.Errorf("skipped_by = %v, want the absurd candidate rejected", sum.SkippedBy)
	}
	art := readArtifact(t, e.c, recs[0])
	if !bytes.Equal(art, data) || !bytes.Equal(art, readImageRuns(e.img, recs[0].Source.Derived.Runs)) {
		t.Error("bytes are not what the image holds at the validated runs")
	}
	d := recs[0].Source.Derived
	if d.Mode != 0o7777 || !d.Encrypted {
		t.Errorf("mode %o encrypted %v: stale claims must be recorded as claimed", d.Mode, d.Encrypted)
	}
	if d.Recovery.Class != evidence.ClassDeletedFile {
		t.Errorf("class = %q: the stale values are labelled by the recovery class", d.Recovery.Class)
	}
}

func TestRecoverRejectsInvalidMaps(t *testing.T) {
	good := pat(1, 2*bs)
	nodes := []fstest.Node{delNode("/a.bin", good), {Path: "/live.bin", Data: pat(2, bs)}}
	lay := layout(t, nodes)
	a := lay["/a.bin"][0]
	imageEnd := int64(len(disk(fstest.Build(fstest.BuildSpec{Label: "L", BlockSize: int(buildBS), FreeBlocks: 4, Nodes: nodes}))))
	tests := []struct {
		name   string
		cand   fstest.RecoverMap
		reason string
		text   string
	}{
		{"negative offset", fstest.RecoverMap{Method: mtfsRecM, Size: bs, Runs: []filesys.Run{{Offset: -5, Length: bs}}}, "invalid-map", "offset"},
		{"hole", fstest.RecoverMap{Method: mtfsRecM, Size: bs, Runs: []filesys.Run{{Offset: -1, Length: bs}}}, "invalid-map", "hole"},
		{"overflow", fstest.RecoverMap{Method: mtfsRecM, Size: bs, Runs: []filesys.Run{{Offset: math.MaxInt64 - 3, Length: 100}}}, "invalid-map", "outside"},
		{"beyond partition", fstest.RecoverMap{Method: mtfsRecM, Size: bs, Runs: []filesys.Run{{Offset: 1 << 40, Length: bs}}}, "invalid-map", "outside"},
		{"beyond image", fstest.RecoverMap{Method: mtfsRecM, Size: bs, Runs: []filesys.Run{{Offset: imageEnd, Length: bs}}}, "invalid-map", "outside"},
		{"overlapping runs", fstest.RecoverMap{Method: mtfsRecM, Size: 2 * bs, Runs: []filesys.Run{{Offset: a.Offset, Length: bs}, {Offset: a.Offset, Length: bs}}}, "invalid-map", "overlap"},
		{"zero length run", fstest.RecoverMap{Method: mtfsRecM, Size: bs, Runs: []filesys.Run{{Offset: a.Offset, Length: 0}}}, "invalid-map", "length"},
		{"sum over size", fstest.RecoverMap{Method: mtfsRecM, Size: bs, Runs: []filesys.Run{{Offset: a.Offset, Length: 2 * bs}}}, "invalid-map", "more than"},
		{"size over partition", fstest.RecoverMap{Method: mtfsRecM, Size: 1 << 40, Runs: []filesys.Run{{Offset: a.Offset, Length: bs}}}, "invalid-map", "exceeds"},
		{"unknown method", fstest.RecoverMap{Method: "zz-made-up", Size: bs, Runs: []filesys.Run{{Offset: a.Offset, Length: bs}}}, "unknown-method", "zz-made-up"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := slices.Clone(nodes)
			n[0].Recover = []fstest.RecoverMap{tc.cand}
			e := newRecEnv(t, n...)
			sum := e.recover(t, examine.RecoverOptions{All: true})
			if got := len(recovered(t, e.c)); got != 0 {
				t.Fatalf("%d artifacts written for an invalid map", got)
			}
			if sum.SkippedBy[tc.reason] != 1 {
				t.Errorf("skipped_by = %v, want %s", sum.SkippedBy, tc.reason)
			}
			var hit bool
			for _, r := range warnReasons(t, e.c) {
				hit = hit || (strings.Contains(r, tc.reason) && strings.Contains(strings.ToLower(r), tc.text))
			}
			if !hit {
				t.Errorf("warnings %q lack %q and %q", warnReasons(t, e.c), tc.reason, tc.text)
			}
		})
	}
}

func TestRecoverCapsCandidatesPerEntry(t *testing.T) {
	maps := make([]fstest.RecoverMap, filesys.MaxCandidatesPerEntry+1)
	for i := range maps {
		maps[i] = fstest.RecoverMap{Method: mtfsRecM}
	}
	e := newRecEnv(t, delNode("/a.bin", pat(1, bs), maps...))
	plan, err := e.s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true, AllCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || len(plan.Items[0].Candidates) != filesys.MaxCandidatesPerEntry {
		t.Fatalf("plan = %+v, want the first 16 candidates", plan.Items)
	}
	e.recover(t, examine.RecoverOptions{All: true, AllCandidates: true})
	if !anyContains(warnReasons(t, e.c), fmt.Sprint(filesys.MaxCandidatesPerEntry)) {
		t.Errorf("warnings = %q, want one saying the list was cut at 16", warnReasons(t, e.c))
	}
}

func TestRecoverOverlappingCandidates(t *testing.T) {
	a := pat(1, 2*bs)
	nodes := []fstest.Node{delNode("/a.bin", a), delNode("/b.bin", pat(2, 2*bs))}
	lay := layout(t, nodes)
	nodes[1].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 2 * bs, Runs: lay["/a.bin"]}}
	e := newRecEnv(t, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != 2 || sum.Overlap != 2 {
		t.Fatalf("%d artifacts, summary %+v: both overlapping candidates must be kept", len(recs), sum)
	}
	ids := map[string]string{}
	for _, r := range recs {
		ids[r.Source.Derived.FSPath] = r.Source.Derived.FSID
	}
	for _, r := range recs {
		other := ids["/a.bin"]
		if r.Source.Derived.FSPath == "/a.bin" {
			other = ids["/b.bin"]
		}
		rv := r.Source.Derived.Recovery
		if !slices.Contains(rv.Assumptions, "overlap="+other) || *rv.Confidence != 30 {
			t.Errorf("%s: assumptions %q confidence %d, want overlap=%s and 30", r.Source.Derived.FSPath, rv.Assumptions, *rv.Confidence, other)
		}
	}
}

func TestRecoverBestCandidateOnlyUnlessAll(t *testing.T) {
	n := delNode("/a.bin", pat(1, 2*bs), fstest.RecoverMap{Method: mtfsRecM}, fstest.RecoverMap{Method: "ext-inode-intact"})
	e := newRecEnv(t, n)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != 1 || recs[0].Source.Derived.Recovery.Method != "ext-inode-intact" || sum.SkippedBy["not-selected"] != 1 {
		t.Fatalf("artifacts %d summary %+v, want only the best (ext-inode-intact, 80)", len(recs), sum)
	}
	e2 := newRecEnv(t, n)
	e2.recover(t, examine.RecoverOptions{All: true, AllCandidates: true})
	if got := len(recovered(t, e2.c)); got != 2 {
		t.Fatalf("%d artifacts with AllCandidates, want 2", got)
	}
	// a same-entry alternative is not an overlap
	for _, r := range recovered(t, e2.c) {
		if anyContains(r.Source.Derived.Recovery.Assumptions, "overlap=") || *r.Source.Derived.Recovery.Confidence == 30 {
			t.Errorf("alternatives of one entry reported as overlap: %q", r.Source.Derived.Recovery.Assumptions)
		}
	}
}

func TestRecoverMinConfidenceFilters(t *testing.T) {
	nodes := []fstest.Node{
		delNode("/hi.bin", pat(1, bs), fstest.RecoverMap{Method: "ext-inode-intact"}),
		delNode("/lo.bin", pat(2, bs), fstest.RecoverMap{Method: "ext4-extent-leaf"}),
	}
	e := newRecEnv(t, nodes...)
	plan, err := e.s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true, MinConfidence: 60})
	if err != nil {
		t.Fatal(err)
	}
	sum := e.recover(t, examine.RecoverOptions{All: true, MinConfidence: 60})
	if sum.Recovered != 1 || sum.SkippedBy["below-min-confidence"] != 1 || plan.SkippedBy["below-min-confidence"] != 1 {
		t.Errorf("run %+v plan %v", sum, plan.SkippedBy)
	}
	if got := recovered(t, e.c); len(got) != 1 || got[0].Source.Derived.Recovery.Method != "ext-inode-intact" {
		t.Errorf("artifacts = %v", got)
	}
}

func TestRecoverSelectionOrder(t *testing.T) {
	// ext4-extent-leaf is 55; two entries claim the same blocks, so each is capped to 30 and a minimum
	// of 40 then filters both (the overlap cap comes before the minimum).
	a := pat(1, bs)
	nodes := []fstest.Node{
		delNode("/a.bin", a, fstest.RecoverMap{Method: "ext4-extent-leaf"}),
		delNode("/b.bin", pat(2, bs), fstest.RecoverMap{Method: "ext4-extent-leaf"}),
	}
	lay := layout(t, nodes)
	nodes[1].Recover[0].Runs = lay["/a.bin"]
	nodes[1].Recover[0].Size = bs
	e := newRecEnv(t, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true, MinConfidence: 40})
	if sum.Recovered != 0 || sum.SkippedBy["below-min-confidence"] != 2 {
		t.Errorf("summary = %+v, want both filtered after the overlap cap", sum)
	}
}

func TestRecoverEntryAndPlanRunCaps(t *testing.T) {
	const capN = 3
	for _, kind := range []string{"max-entries", "max-plan-runs"} {
		for _, n := range []int{capN - 1, capN, capN + 1} {
			t.Run(fmt.Sprintf("%s/%d", kind, n), func(t *testing.T) {
				if kind == "max-entries" {
					defer examine.SetRecoverCaps(capN, 1<<30)()
				} else {
					defer examine.SetRecoverCaps(1<<30, capN)()
				}
				var nodes []fstest.Node
				for i := range n {
					nodes = append(nodes, delNode(fmt.Sprintf("/f%d.bin", i), pat(byte(i+1), bs)))
				}
				e := newRecEnv(t, nodes...)
				sum := e.recover(t, examine.RecoverOptions{All: true})
				over := n > capN
				wantLimit, wantNot, wantDone := "", 0, n
				if over {
					wantLimit, wantNot, wantDone = kind, n-capN, capN
				}
				if sum.LimitReached != wantLimit || sum.NotProcessed != wantNot || sum.Recovered != wantDone {
					t.Errorf("limit %q not processed %d recovered %d, want %q %d %d", sum.LimitReached, sum.NotProcessed, sum.Recovered, wantLimit, wantNot, wantDone)
				}
				limitWarnings := 0
				for _, r := range warnReasons(t, e.c) {
					if strings.Contains(r, "limit") {
						limitWarnings++
					}
				}
				if (over && limitWarnings != 1) || (!over && limitWarnings != 0) {
					t.Errorf("%d limit warnings", limitWarnings)
				}
				if over && sum.SkippedBy["limit"] != n-capN {
					t.Errorf("skipped_by = %v", sum.SkippedBy)
				}
			})
		}
	}
}

// Maps the MTFS table cannot hold (a million runs) or the builder normalises away (a size with no runs)
// are injected through the Recoverer.
func TestRecoverRejectsInjectedMaps(t *testing.T) {
	many := make([]filesys.Run, filesys.MaxCandidateRuns+1)
	for i := range many {
		many[i] = filesys.Run{Offset: 4096, Length: 1}
	}
	tests := []struct {
		name   string
		cand   filesys.Candidate
		reason string
		text   string
	}{
		{"too many runs", filesys.Candidate{Method: mtfsRecM, Size: int64(len(many)), Runs: many}, "invalid-map", "too many runs"},
		{"size but no runs", filesys.Candidate{Method: mtfsRecM, Size: bs}, "no-runs", "no runs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newHookEnv(t, func(h *fsHook) {
				h.recoverable = func(filesys.Recoverer, filesys.Entry) ([]filesys.Candidate, error) {
					return []filesys.Candidate{tc.cand}, nil
				}
			}, delNode("/a.bin", pat(1, bs)))
			sum := e.recover(t, examine.RecoverOptions{All: true})
			if got := len(recovered(t, e.c)); got != 0 {
				t.Fatalf("%d artifacts written", got)
			}
			if sum.SkippedBy[tc.reason] != 1 || !anyContains(warnReasons(t, e.c), tc.text) {
				t.Errorf("skipped_by %v warnings %q, want %s / %q", sum.SkippedBy, warnReasons(t, e.c), tc.reason, tc.text)
			}
		})
	}
}

// C57: with more than 16 overlapping partners the count of the further overlapping RUNS is reported.
func TestRecoverOverlapOtherRunsIsExact(t *testing.T) {
	const n = 20
	buildBS = 16384 // the table of 20 maps does not fit one 4 KiB block
	t.Cleanup(func() { buildBS = bs })
	nodes := []fstest.Node{delNode("/e00.bin", pat(1, bs))}
	for i := 1; i < n; i++ {
		nodes = append(nodes, delNode(fmt.Sprintf("/e%02d.bin", i), pat(byte(i+1), bs)))
	}
	lay := layout(t, nodes)
	for i := 1; i < n; i++ {
		nodes[i].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: bs, Runs: lay["/e00.bin"]}}
	}
	e := newRecEnv(t, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != n {
		t.Fatalf("%d artifacts, want %d: %+v", len(recs), n, sum)
	}
	for _, r := range recs {
		as := r.Source.Derived.Recovery.Assumptions
		listed := 0
		for _, a := range as {
			if strings.HasPrefix(a, "overlap=") {
				listed++
			}
		}
		if listed != 16 || !slices.Contains(as, "overlap-other-runs=3") || anyContains(as, "overlap-listed") {
			t.Errorf("%s: %d listed, assumptions %q, want 16 listed and overlap-other-runs=3", r.Source.Derived.FSPath, listed, as)
		}
	}
}

// C53: a candidate of size 0 is skipped as empty, not as having no free bytes.
func TestRecoverEmptyCandidateIsEmpty(t *testing.T) {
	e := newRecEnv(t, delNode("/z.bin", nil))
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if sum.SkippedBy["empty"] != 1 || sum.SkippedBy["no-free-bytes"] != 0 || sum.Recovered != 0 {
		t.Errorf("summary %+v, want one empty skip", sum)
	}
}
