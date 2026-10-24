package examine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
)

// allocationMaxProblems is how many allocation problems are listed; the rest is one line.
const allocationMaxProblems = 50

// AllocationCheck is R9: every deleted-file artifact's recorded runs lie inside the space its filesystem
// reports unallocated, recomputed with the reader the recovery used. drivers are the filesystem drivers
// (nil = detect.Drivers; the command line passes Deps.FSDrivers, so test drivers are seen exactly as by
// image recover). No option disables it. Carved, slack and journal artifacts are held by R5 only.
func AllocationCheck(drivers []detect.Driver) evidence.VerifyCheck {
	return func(ctx context.Context, c *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport) {
		verifyAllocation(ctx, c, recs, rep, drivers)
	}
}

// allocationArtifactHook sees the id of each artifact at the start of its check (a test seam).
var allocationArtifactHook func(id string)

type allocGroup struct {
	parent    string
	partition int
	offset    int64
	fsType    string
}

type allocator struct {
	c       *evidence.Case
	rep     *evidence.VerifyReport
	byID    map[string]evidence.ManifestRecord
	listed  int
	dropped int
	add     func(n int64)
}

func (a *allocator) problem(rec evidence.ManifestRecord, format string, args ...any) {
	if a.listed >= allocationMaxProblems {
		a.dropped++
		return
	}
	a.listed++
	a.rep.AddProblem("allocation: %s (artifact %q, %q)", fmt.Sprintf(format, args...), rec.ID, rec.Path)
}

func verifyAllocation(ctx context.Context, c *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport, drivers []detect.Driver) {
	rep.MarkAllocationRan()
	a := &allocator{c: c, rep: rep, byID: make(map[string]evidence.ManifestRecord, len(recs)), add: func(int64) {}}
	groups := map[allocGroup][]evidence.ManifestRecord{}
	var total int64
	for _, rec := range recs {
		if _, dup := a.byID[rec.ID]; dup {
			continue // a duplicate id is reported once by the manifest checks
		}
		a.byID[rec.ID] = rec
		d := rec.Source.Derived
		if rec.Source.Kind != evidence.KindRecover || d == nil || d.Recovery == nil || d.Recovery.Class != evidence.ClassDeletedFile {
			continue
		}
		k := allocGroup{d.ParentID, d.Partition, d.PartitionOffset, d.FSType}
		groups[k] = append(groups[k], rec)
		total++
	}
	done, add := c.VerifyPhase("allocation", total, "artifacts")
	defer done()
	a.add = add
	keys := make([]allocGroup, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y allocGroup) int {
		if c := strings.Compare(x.parent, y.parent); c != 0 {
			return c
		}
		if x.partition != y.partition {
			return x.partition - y.partition
		}
		if x.offset != y.offset {
			if x.offset < y.offset {
				return -1
			}
			return 1
		}
		return strings.Compare(x.fsType, y.fsType)
	})
	var (
		sess   *Session
		sessID string
		sessEr error
	)
	closeSess := func() {
		if sess != nil {
			_ = sess.Close()
			sess = nil
		}
	}
	defer closeSess()
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		g := groups[k]
		slices.SortFunc(g, func(x, y evidence.ManifestRecord) int {
			if c := strings.Compare(x.Path, y.Path); c != 0 {
				return c
			}
			return strings.Compare(x.ID, y.ID)
		})
		if sessID != k.parent || (sess == nil && sessEr == nil) {
			closeSess()
			sess, sessEr = Open(c, k.parent, Options{Drivers: drivers})
			sessID = k.parent
		}
		if !a.group(ctx, sess, sessEr, k, g) {
			break
		}
	}
	if a.dropped > 0 {
		rep.AddProblem("allocation: %d further problems are not listed", a.dropped)
	}
}

// group rechecks the artifacts that name one (parent, partition, offset, filesystem). It returns false when
// the context ended; the findings made so far are kept.
func (a *allocator) group(ctx context.Context, s *Session, openErr error, k allocGroup, g []evidence.ManifestRecord) bool {
	skip := func(format string, args ...any) bool {
		a.problem(g[0], format+" (%d artifact(s) not rechecked)", append(args, len(g))...)
		a.add(int64(len(g)))
		return true
	}
	if openErr != nil {
		return skip("partition %d of parent %q cannot be opened: %v", k.partition, k.parent, openErr)
	}
	fsys, part, err := s.FS(k.partition)
	if err != nil {
		return skip("partition %d of parent %q cannot be opened: %v", k.partition, k.parent, err)
	}
	if part.Start != k.offset {
		return skip("partition %d starts at %d, the artifact records %d", k.partition, part.Start, k.offset)
	}
	if t := fsys.Info().Type; t != k.fsType {
		return skip("the filesystem is %q, the artifact records %q", t, k.fsType)
	}
	// the free map is built exactly as the recovery plan builds it
	runs, _, err := s.unallocatedRunsNote(fsys, part, false)
	if err != nil {
		return skip("the unallocated space of partition %d cannot be listed: %v", k.partition, err)
	}
	free := newFreeMap(runs, len(fsys.Info().Warnings) > 0)
	for _, rec := range g {
		if allocationArtifactHook != nil {
			allocationArtifactHook(rec.ID)
		}
		if ctx.Err() != nil {
			return false
		}
		a.one(rec, free)
		a.add(1)
	}
	return true
}

func (a *allocator) one(rec evidence.ManifestRecord, free *freeMap) {
	runs, err := a.c.DerivedRuns(rec, a.byID)
	if err != nil {
		a.problem(rec, "runs unreadable: %v", err)
		return
	}
	checked := true
	for i, r := range runs {
		if r.Offset < 0 || r.Length <= 0 {
			checked = false // holes, empty and negative runs are reported by the runs check
			continue
		}
		if !free.contains(r.Offset, r.Length) {
			a.problem(rec, "run %d (%d+%d) lies outside the space the filesystem reports unallocated", i, r.Offset, r.Length)
			checked = false
		}
	}
	if checked {
		a.rep.AllocationRechecked++
	}
}
