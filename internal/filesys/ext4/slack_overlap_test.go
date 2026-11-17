package ext4_test

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// sharesBlocks reports whether two data runs of runs name the same block (compared as whole blocks of unit bytes).
func sharesBlocks(runs []filesys.Run, unit int64) bool {
	u := filesys.UnitRuns(runs, unit)
	sort.SliceStable(u, func(i, j int) bool { return u[i].Offset < u[j].Offset })
	for i := 1; i < len(u); i++ {
		if u[i-1].Offset+u[i-1].Length > u[i].Offset {
			return true
		}
	}
	return false
}

// checkSlackOutcome asserts what the slack export relies on for one file whose runs are valid: the allocation is the
// contract (filesys.CheckAllocatedRuns), unless a hostile extent tree maps one block twice. The reader reports such a
// file's runs as they are (the tree is the evidence) and the allocation cannot then be disjoint; the consumer
// (filesys.TailSlack) must never derive slack from it: it refuses with SlackAllocMismatch, or earlier (a trailing hole, a prefix) with no slack.
func checkSlackOutcome(t *testing.T, path string, file filesys.File, alloc []filesys.Run, info filesys.Info) {
	t.Helper()
	err := filesys.CheckAllocatedRuns(file.Runs(), alloc, int64(info.BlockSize), info.Size)
	if err == nil {
		return
	}
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "overlap") || !sharesBlocks(file.Runs(), int64(info.BlockSize)) || !sharesBlocks(alloc, int64(info.BlockSize)) {
		t.Fatalf("%s: runs %v alloc %v: %v", path, file.Runs(), alloc, err)
	}
	if _, outcome, _ := filesys.TailSlack(file.Size(), file.Runs(), alloc, int64(info.BlockSize), info.Size); outcome == filesys.SlackFound || outcome == filesys.SlackExactFit {
		t.Fatalf("%s: runs %v share a block but TailSlack says %q, want a refusal", path, file.Runs(), outcome)
	}
}

// TestExt4SharedBlocksGiveNoSlack: two extents of one file that map the same physical blocks are read as the tree
// says, and the slack of that file is refused with a typed outcome, never derived.
func TestExt4SharedBlocksGiveNoSlack(t *testing.T) {
	h := newHostile(t)
	put16(h.img, h.ib0+2, 2)
	put32(h.img, h.ib0+24, 8) // logically after the first extent, physically inside it
	put16(h.img, h.ib0+24+4, 2)
	put32(h.img, h.ib0+24+8, le32(h.img, h.ib0+12+8)+1)
	put32(h.img, h.ino0+4, 10*uint32(h.bs)-5) // the last extent ends mid-block: slack would exist if the runs were trusted
	f := mustOpen(t, h.img)
	file := openNum(t, f, ext4test.InodeNumber(0))
	alloc, ok := filesys.AllocatedRunsOf(file)
	if !ok {
		t.Fatal("no AllocatedRunner")
	}
	if !sharesBlocks(file.Runs(), int64(f.Info().BlockSize)) {
		t.Fatalf("the mutation did not make the runs share a block: %v", file.Runs())
	}
	if err := filesys.CheckAllocatedRuns(file.Runs(), alloc, int64(f.Info().BlockSize), f.Info().Size); err == nil {
		t.Fatal("overlapping allocation passed CheckAllocatedRuns")
	}
	checkSlackOutcome(t, "/t", file, alloc, f.Info())
}
