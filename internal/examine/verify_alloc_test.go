package examine_test

import (
	"context"
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
)

type allocEnv struct {
	s    *examine.Session
	img  []byte // the imported disk
	sum  examine.RecoverSummary
	real evidence.ManifestRecord // one real deleted-file recovery
}

func deletedFiles(recs []evidence.ManifestRecord) (n int) {
	for _, r := range recs {
		if r.Source.Kind == evidence.KindRecover && r.Source.Derived != nil && r.Source.Derived.Recovery != nil && r.Source.Derived.Recovery.Class == evidence.ClassDeletedFile {
			n++
		}
	}
	return n
}

// allocFixture imports a kernel-written recovery fixture and recovers everything the reader offers.
func allocFixture(t *testing.T, dir, name string) allocEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("imports and recovers a real-image fixture")
	}
	img, _ := loadFixRecover(t, dir, name)
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	e := allocEnv{s: s, img: disk, sum: sum}
	for _, a := range sum.Artifacts {
		if a.Source.Kind == evidence.KindRecover {
			e.real = a
			break
		}
	}
	if e.real.ID == "" {
		t.Fatal("the fixture recovered nothing")
	}
	return e
}

func allocVerify(ctx context.Context, t *testing.T, c *evidence.Case) evidence.VerifyReport {
	t.Helper()
	rep, err := c.VerifyWith(ctx, examine.VerifyChecks(examine.Options{})...)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func noAllocationProblem(t *testing.T, rep evidence.VerifyReport) {
	t.Helper()
	for _, p := range rep.Problems {
		if strings.Contains(p, "allocation: ") {
			t.Errorf("unexpected allocation problem: %s", p)
		}
	}
}

func TestAllocationRecheckCleanOnKernelFixtures(t *testing.T) {
	for _, f := range []struct{ dir, name string }{
		{"fat", "fat12-recover"}, {"fat", "fat16-recover"}, {"fat", "fat32-recover"}, {"exfat", "exfat-recover"},
	} {
		t.Run(f.name, func(t *testing.T) {
			e := allocFixture(t, f.dir, f.name)
			rep := allocVerify(context.Background(), t, e.s.Case)
			noAllocationProblem(t, rep)
			evidencetest.RequireClean(t, rep)
			want := deletedFiles(e.sum.Artifacts)
			if want == 0 || rep.AllocationRechecked != want {
				t.Errorf("AllocationRechecked = %d, want the %d deleted-file artifacts written", rep.AllocationRechecked, want)
			}
		})
	}
}

// forgedSpec is a deleted-file artifact on the real recoveries' parent, partition and filesystem whose runs
// are the given image-relative runs, its bytes being the image's own (so R5 and R6 have nothing to say).
func (e allocEnv) forge(t *testing.T, name string, runs []evidence.Run, mutate func(*evidencetest.RecoveredSpec)) evidence.ManifestRecord {
	t.Helper()
	d := e.real.Source.Derived
	parent, err := e.s.Case.FindArtifact(d.ParentID)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, r := range runs {
		n += r.Length
	}
	spec := evidencetest.RecoveredSpec{
		Partition: d.Partition, PartitionOffset: d.PartitionOffset,
		FSType: d.FSType, Runs: runs, Alloc: &evidence.AllocSummary{Free: n},
	}
	if mutate != nil {
		mutate(&spec)
	}
	spec.Path = path.Join("recovered", fmt.Sprintf("p%d-%s", spec.Partition, spec.FSType), name)
	return evidencetest.AddRecovered(t, e.s.Case, parent, e.img, spec)
}

// allocatedRun is a short image-relative run of the first filesystem bytes the reader does not report free.
func (e allocEnv) allocatedRun(t *testing.T) evidence.Run {
	t.Helper()
	fsys, part, err := e.s.FS(e.real.Source.Derived.Partition)
	if err != nil {
		t.Fatal(err)
	}
	free, err := fsys.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	var at int64
	for _, r := range free {
		if r.Offset > at {
			break
		}
		at = max(at, r.Offset+r.Length)
	}
	// at is the first byte not in a free run that starts at or before it: the allocated part below the first free run, or a gap
	if len(free) > 0 && free[0].Offset > 0 {
		at = 0
	}
	return evidence.Run{Offset: part.Start + at, Length: 512}
}

func TestAllocationRecheckFlagsRunsInAllocatedSpace(t *testing.T) {
	e := allocFixture(t, "fat", "fat12-recover")
	run := e.allocatedRun(t)
	e.forge(t, "900001-live.bin", []evidence.Run{run}, nil)
	rep := allocVerify(context.Background(), t, e.s.Case)
	for _, p := range rep.Problems {
		if strings.Contains(p, "reproduce: ") {
			t.Errorf("R6 must be clean for the forgery: %s", p)
		}
	}
	evidencetest.RequireProblem(t, rep, "allocation: run 0 (")
	if rep.AllocationRechecked != deletedFiles(e.sum.Artifacts) {
		t.Errorf("AllocationRechecked = %d, want only the real %d (the forgery is not counted)", rep.AllocationRechecked, deletedFiles(e.sum.Artifacts))
	}
}

func TestAllocationRecheckMismatchedPartitionAndFS(t *testing.T) {
	e := allocFixture(t, "fat", "fat12-recover")
	d := e.real.Source.Derived
	runs := e.real.Source.Derived.Runs
	if len(runs) == 0 {
		t.Skip("the first recovery records its runs in a sidecar")
	}
	e.forge(t, "900001-p9.bin", runs, func(s *evidencetest.RecoveredSpec) { s.Partition = 9 })
	e.forge(t, "900002-off.bin", runs, func(s *evidencetest.RecoveredSpec) { s.PartitionOffset = d.PartitionOffset + 512 })
	e.forge(t, "900003-fs.bin", runs, func(s *evidencetest.RecoveredSpec) { s.FSType = "ext4" })
	rep := allocVerify(context.Background(), t, e.s.Case)
	evidencetest.RequireProblem(t, rep, "allocation: partition 9 of parent")
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("allocation: partition %d starts at %d, the artifact records %d", d.Partition, d.PartitionOffset, d.PartitionOffset+512))
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("allocation: the filesystem is %q, the artifact records %q", d.FSType, "ext4"))
}

func TestAllocationRecheckCancelKeepsFindings(t *testing.T) {
	e := allocFixture(t, "fat", "fat12-recover")
	run := e.allocatedRun(t)
	first := e.forge(t, "000000-live.bin", []evidence.Run{run}, nil)
	later := e.forge(t, "000001-live.bin", []evidence.Run{run}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer examine.SetAllocationArtifactHook(func(id string) {
		if id == later.ID { // the first forgery was checked just before; end the verify before this one
			cancel()
		}
	})()
	rep, err := e.s.Case.VerifyWith(ctx, examine.AllocationCheck(nil))
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("lies outside the space the filesystem reports unallocated (artifact %q", first.ID))
	evidencetest.RequireNoProblem(t, rep, later.ID)
	evidencetest.RequireProblem(t, rep, "reproduce: verification cancelled")
	if rep.CancelledOnly() {
		t.Error("a cancelled verify that holds a finding must not be cancel-only (exit 4, never 1)")
	}
}

func TestAllocationNoticeWhenNotComposed(t *testing.T) {
	e := allocFixture(t, "fat", "fat12-recover")
	rep, err := e.s.Case.Verify()
	if err != nil {
		t.Fatal(err)
	}
	noAllocationProblem(t, rep)
	evidencetest.RequireNotice(t, rep, fmt.Sprintf("%d deleted-file artifact(s): allocation not rechecked against the filesystem", deletedFiles(e.sum.Artifacts)))
	if rep.AllocationRechecked != 0 {
		t.Errorf("AllocationRechecked = %d without the check", rep.AllocationRechecked)
	}
	rep = allocVerify(context.Background(), t, e.s.Case)
	for _, n := range rep.Notices {
		if strings.Contains(n, "allocation not rechecked") {
			t.Errorf("notice %q although R9 ran", n)
		}
	}
}
