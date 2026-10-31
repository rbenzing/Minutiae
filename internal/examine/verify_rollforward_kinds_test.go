package examine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// journalBlockSpec is a journal-block copy of the given method on partition 1 of an mtfs image.
func journalBlockSpec(method string, runs []evidence.Run) evidencetest.RecoveredSpec {
	return evidencetest.RecoveredSpec{
		Kind: evidence.KindJournal, Class: evidence.ClassJournalBlock, Method: method, Path: "journal/p1-mtfs/seq1-fsblk2.bin",
		Runs: runs, Confidence: nil,
		Mutate: func(r *evidence.Recovery) {
			r.Confidence = nil
			r.Journal = &evidence.JournalRef{Seq: 1, Committed: true, Region: "live"}
		},
	}
}

// 3H-R13 c: R6 reproduces the journal block copies, and the notice for kind journal is gone once R6 ran.
func TestVerifyJournalBlockReproduced(t *testing.T) {
	runs := []evidence.Run{{Offset: 8192, Length: 4096}}
	t.Run("clean", func(t *testing.T) {
		c, parent, img := r6Image(t, 256<<10)
		evidencetest.AddRecovered(t, c, parent, img, journalBlockSpec("f2fs-rollforward", runs))
		rep := r6Verify(t, c)
		r6NoReproduceProblem(t, rep)
		if rep.RecoveredReproduced != 1 {
			t.Errorf("reproduced = %d, want 1", rep.RecoveredReproduced)
		}
		for _, n := range rep.Notices {
			if strings.Contains(n, `kind "journal"`) {
				t.Errorf("notice for kind journal remains after R6 ran: %s", n)
			}
		}
	})
	t.Run("flipped byte", func(t *testing.T) {
		c, parent, img := r6Image(t, 256<<10)
		data := append([]byte(nil), img[8192:8192+4096]...)
		data[100] ^= 1
		spec := journalBlockSpec("f2fs-rollforward", runs)
		spec.Data = data
		evidencetest.AddRecovered(t, c, parent, img, spec)
		rep := r6Verify(t, c)
		evidencetest.RequireProblem(t, rep, "reproduce: byte 100 of the artifact differs from image offset 8292")
		if rep.RecoveredReproduced != 0 {
			t.Errorf("reproduced = %d, want 0", rep.RecoveredReproduced)
		}
	})
}

// rfAllocEnv is a case holding a GPT disk with an mtfs partition and the partition's free and allocated runs.
type rfAllocEnv struct {
	c           *evidence.Case
	parent      evidence.ManifestRecord
	img         []byte
	partStart   int64
	free, alloc evidence.Run // image-relative
}

func newRFAllocEnv(t *testing.T) rfAllocEnv {
	t.Helper()
	c := newCase(t)
	img := disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: []fstest.Node{{Path: "/a.txt", Data: []byte("hello")}}}))
	recs := importImage(t, c, img, 1)
	s := openSession(t, c, recs[0].ID)
	fsys, part, err := s.FS(1)
	if err != nil {
		t.Fatal(err)
	}
	un, err := fsys.Unallocated()
	if err != nil || len(un) == 0 {
		t.Fatalf("Unallocated = %v, %v", un, err)
	}
	if un[0].Offset == 0 {
		t.Fatal("the first filesystem bytes are free: no allocated run to test with")
	}
	return rfAllocEnv{
		c: c, parent: recs[0], img: img, partStart: part.Start,
		free:  evidence.Run{Offset: part.Start + un[0].Offset, Length: 512},
		alloc: evidence.Run{Offset: part.Start, Length: 512},
	}
}

func (e rfAllocEnv) add(t *testing.T, spec evidencetest.RecoveredSpec) evidence.ManifestRecord {
	t.Helper()
	spec.PartitionOffset = e.partStart
	return evidencetest.AddRecovered(t, e.c, e.parent, e.img, spec)
}

func (e rfAllocEnv) verify(t *testing.T) evidence.VerifyReport {
	t.Helper()
	rep, err := e.c.VerifyWith(context.Background(), examine.AllocationCheck(mtfsOpts().Drivers))
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func rfFileSpec(name string, run evidence.Run) evidencetest.RecoveredSpec {
	conf := 60
	return evidencetest.RecoveredSpec{
		Class: evidence.ClassPostCheckpointFile, Method: "f2fs-rollforward", Confidence: &conf,
		Path: "recovered/p1-mtfs/" + name, Runs: []evidence.Run{run},
	}
}

// 3H-R13 d: R9 rechecks post-checkpoint files, and journal blocks of method f2fs-rollforward, against the
// free space; ext4 journal blocks are left to plan 3G.
func TestVerifyAllocationRechecksPostCheckpoint(t *testing.T) {
	e := newRFAllocEnv(t)
	good := e.add(t, rfFileSpec("000001-free.bin", e.free))
	bad := e.add(t, rfFileSpec("000002-live.bin", e.alloc))
	jfree := journalBlockSpec("f2fs-rollforward", []evidence.Run{e.free})
	jfree.Path = "journal/p1-mtfs/seq1-free.bin"
	e.add(t, jfree)
	jbad := journalBlockSpec("f2fs-rollforward", []evidence.Run{e.alloc})
	jbad.Path = "journal/p1-mtfs/seq1-alloc.bin"
	jbadRec := e.add(t, jbad)
	jext4 := journalBlockSpec("ext4-journal", []evidence.Run{e.alloc})
	jext4.Path = "journal/p1-mtfs/seq9-ext4.bin"
	ext4Rec := e.add(t, jext4)

	rep := e.verify(t)
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("allocation: run 0 (%d+512) lies outside the space the filesystem reports unallocated (artifact %q", e.alloc.Offset, bad.ID))
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("(artifact %q", jbadRec.ID))
	evidencetest.RequireNoProblem(t, rep, fmt.Sprintf("(artifact %q", good.ID))
	evidencetest.RequireNoProblem(t, rep, fmt.Sprintf("(artifact %q", ext4Rec.ID))
	if rep.AllocationRechecked != 2 {
		t.Errorf("AllocationRechecked = %d, want the 2 clean artifacts in free space", rep.AllocationRechecked)
	}
}
