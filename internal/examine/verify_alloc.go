package examine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/volume"
)

// allocationMaxProblems is how many allocation problems are listed; the rest is one line.
const allocationMaxProblems = 50

// AllocationCheck is R9: the recorded runs of every deleted-file artifact, and of every carved artifact whose
// scope reads a container (unallocated, volume, raw), are rechecked against the image with the reader the
// recovery used: deleted-file and unallocated runs lie inside the space the filesystem reports unallocated,
// volume runs inside the table's unallocated gaps, and a raw carve's allocation counts and region equal the
// classifier's. drivers are the filesystem drivers (nil = detect.Drivers; the command line passes
// Deps.FSDrivers, so test drivers are seen exactly as by image recover). No option disables it. A carve of
// scope artifact has no allocation state; slack and journal artifacts are held by R5 only.
func AllocationCheck(drivers []detect.Driver) evidence.VerifyCheck {
	return func(ctx context.Context, c *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport) {
		verifyAllocation(ctx, c, recs, rep, drivers)
	}
}

// allocationArtifactHook sees the id of each artifact at the start of its check (a test seam).
var allocationArtifactHook func(id string)

// allocRule is how R9 rechecks one artifact.
type allocRule int

const (
	allocNone   allocRule = iota // not rechecked: not a recovery from a container, or a carve of scope artifact
	allocFS                      // runs lie in the space the filesystem reports unallocated (deleted-file, carve unallocated)
	allocVolume                  // runs lie in the partition table's unallocated gaps (carve volume)
	allocRaw                     // allocation counts and region equal the classifier's (carve raw)
)

// allocRuleOf says which R9 rule applies to rec.
func allocRuleOf(rec evidence.ManifestRecord) allocRule {
	d := rec.Source.Derived
	if d == nil || d.Recovery == nil {
		return allocNone
	}
	switch rec.Source.Kind {
	case evidence.KindRecover:
		if d.Recovery.Class == evidence.ClassDeletedFile {
			return allocFS
		}
	case evidence.KindCarve:
		switch d.Recovery.Scope {
		case CarveUnallocated:
			return allocFS
		case CarveVolume:
			return allocVolume
		case CarveRaw:
			return allocRaw
		}
	}
	return allocNone
}

type allocGroup struct {
	rule      allocRule
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
		rule := allocRuleOf(rec)
		if rule == allocNone {
			continue
		}
		// volume and raw carves are decided per image, not per partition: their group has no partition
		k := allocGroup{rule: rule, parent: d.ParentID}
		if rule == allocFS {
			k.partition, k.offset, k.fsType = d.Partition, d.PartitionOffset, d.FSType
		}
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
		if x.rule != y.rule {
			return int(x.rule) - int(y.rule)
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

// group rechecks the artifacts of one group by its rule. It returns false when the context ended; the findings
// made so far are kept.
func (a *allocator) group(ctx context.Context, s *Session, openErr error, k allocGroup, g []evidence.ManifestRecord) bool {
	skip := func(format string, args ...any) bool {
		a.problem(g[0], format+" (%d artifact(s) not rechecked)", append(args, len(g))...)
		a.add(int64(len(g)))
		return true
	}
	each := func(check func(rec evidence.ManifestRecord)) bool {
		for _, rec := range g {
			if allocationArtifactHook != nil {
				allocationArtifactHook(rec.ID)
			}
			if ctx.Err() != nil {
				return false
			}
			check(rec)
			a.add(1)
		}
		return true
	}
	switch k.rule {
	case allocVolume:
		if openErr != nil {
			return skip("parent %q cannot be opened: %v", k.parent, openErr)
		}
		// the gaps are listed exactly as the volume scope lists them
		runs, _, err := s.unallocatedRunsNote(nil, volume.Partition{}, true)
		if err != nil {
			return skip("the unallocated gaps of the volume cannot be listed: %v", err)
		}
		gaps := newFreeMap(runs, false)
		return each(func(rec evidence.ManifestRecord) { a.one(rec, gaps, "the volume's unallocated gaps") })
	case allocRaw:
		if openErr != nil {
			return skip("parent %q cannot be opened: %v", k.parent, openErr)
		}
		cls := newUnknownClassifier(s.Image.Size()) // a table that cannot be read leaves every byte unknown
		if s.Table != nil {
			var err error
			if cls, err = s.newAllocClassifier(); err != nil {
				return skip("the allocation of parent %q cannot be classified: %v", k.parent, err)
			}
		}
		return each(func(rec evidence.ManifestRecord) { a.oneRaw(rec, cls) })
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
	return each(func(rec evidence.ManifestRecord) { a.one(rec, free, "the space the filesystem reports unallocated") })
}

// one checks that every run of rec lies inside free (what names the space in the problem text).
func (a *allocator) one(rec evidence.ManifestRecord, free *freeMap, what string) {
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
			a.problem(rec, "run %d (%d+%d) lies outside %s", i, r.Offset, r.Length, what)
			checked = false
		}
	}
	if checked {
		a.rep.AllocationRechecked++
	}
}

// oneRaw recomputes the allocation counts and the region of a carve of scope raw from the image: they are
// never trusted from the record. The region is the one of the first run; every run must lie in it and in
// one region of the classifier.
func (a *allocator) oneRaw(rec evidence.ManifestRecord, cls *allocClassifier) {
	runs, err := a.c.DerivedRuns(rec, a.byID)
	if err != nil {
		a.problem(rec, "runs unreadable: %v", err)
		return
	}
	if len(runs) == 0 {
		return // reported by the runs check
	}
	var sum evidence.AllocSummary
	var first allocRegion
	for i, r := range runs {
		if r.Offset < 0 || r.Length <= 0 {
			return // holes, empty and negative runs are reported by the runs check
		}
		got, reg, ok := cls.classify(r)
		if !ok {
			a.problem(rec, "run %d (%d+%d) lies outside the image or crosses a region of its allocation", i, r.Offset, r.Length)
			return
		}
		if i == 0 {
			first = reg
		} else if reg.Partition != first.Partition || reg.PartOffset != first.PartOffset || reg.FSType != first.FSType {
			a.problem(rec, "run %d (%d+%d) lies in another region than run 0", i, r.Offset, r.Length)
			return
		}
		sum.Free += got.Free
		sum.Allocated += got.Allocated
		sum.Unknown += got.Unknown
	}
	d := rec.Source.Derived
	ok := true
	if got := d.Recovery.Alloc; got.Free != sum.Free || got.Allocated != sum.Allocated || got.Unknown != sum.Unknown {
		a.problem(rec, "recorded alloc free %d allocated %d unknown %d, recomputed %d %d %d",
			got.Free, got.Allocated, got.Unknown, sum.Free, sum.Allocated, sum.Unknown)
		ok = false
	}
	if d.Partition != first.Partition || d.PartitionOffset != first.PartOffset || d.FSType != first.FSType {
		a.problem(rec, "recorded region partition %d at %d (%q), recomputed partition %d at %d (%q)",
			d.Partition, d.PartitionOffset, d.FSType, first.Partition, first.PartOffset, first.FSType)
		ok = false
	}
	if ok {
		a.rep.AllocationRechecked++
	}
}
