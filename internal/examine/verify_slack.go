package examine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/volume"
)

// slackMaxProblems is how many slack problems are listed; the rest is one line.
const slackMaxProblems = 50

// SlackCheck is R11 (3F-R12, R17, R21): every slack artifact is re-derived from the filesystem it names. A
// complete artifact must equal a re-run of the slack plan on its partition (every exported segment and the
// counters in its Params); an incomplete one (walk errors, a limit, a copy that failed) is checked segment by
// segment: each file segment is exactly the tail slack of the file its fs_id names now, not free, with the
// recorded size, each omitted range is uniform in the image, and a shortened last line is only a prefix of the
// line it was cut from. Both forms: no two segments overlap, the uniform flags equal the bytes, and a
// volume segment lies past the end of the filesystem inside the partition. drivers are the filesystem drivers
// (nil = detect.Drivers); no option disables the check, and a problem is never a pass.
func SlackCheck(drivers []detect.Driver) evidence.VerifyCheck {
	return func(ctx context.Context, c *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport) {
		verifySlack(ctx, c, recs, rep, drivers)
	}
}

// slackArtifactHook sees the id of each artifact at the start of its check (a test seam).
var slackArtifactHook func(id string)

type slackGroup struct {
	parent    string
	partition int
	offset    int64
	fsType    string
}

type slackVerifier struct {
	ctx     context.Context
	c       *evidence.Case
	rep     *evidence.VerifyReport
	ix      *evidence.ManifestIndex
	listed  int
	dropped int
	raised  int // problems raised so far, listed or not: an artifact is rechecked when this did not move
	add     func(n int64)
	buf     []byte
}

func (v *slackVerifier) problem(rec evidence.ManifestRecord, format string, args ...any) {
	v.raised++
	if v.listed >= slackMaxProblems {
		v.dropped++
		return
	}
	v.listed++
	v.rep.AddProblem("slack: %s (artifact %q, %q)", fmt.Sprintf(format, args...), rec.ID, rec.Path)
}

func verifySlack(ctx context.Context, c *evidence.Case, recs []evidence.ManifestRecord, rep *evidence.VerifyReport, drivers []detect.Driver) {
	rep.MarkSlackRan()
	v := &slackVerifier{ctx: ctx, c: c, rep: rep, ix: evidence.NewManifestIndex(recs), add: func(int64) {}, buf: make([]byte, uniformChunk)}
	groups := map[slackGroup][]evidence.ManifestRecord{}
	seen := make(map[string]bool, len(recs))
	var total int64
	for _, rec := range recs {
		if seen[rec.ID] {
			continue // a duplicate id is reported once by the manifest checks
		}
		seen[rec.ID] = true
		d := rec.Source.Derived
		if rec.Source.Kind != evidence.KindSlack || d == nil || d.Recovery == nil {
			continue
		}
		if m := d.Recovery.Method; m != evidence.MethodSlackFile && m != evidence.MethodSlackVolume {
			continue // a wrong method is a problem of R2
		}
		k := slackGroup{d.ParentID, d.Partition, d.PartitionOffset, d.FSType}
		groups[k] = append(groups[k], rec)
		total++
	}
	done, add := c.VerifyPhase("slack", total, "artifacts")
	defer done()
	v.add = add
	keys := make([]slackGroup, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y slackGroup) int {
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
		if !v.group(sess, sessEr, k, g) {
			break
		}
	}
	if v.dropped > 0 {
		rep.AddProblem("slack: %d further problems are not listed", v.dropped)
	}
}

// slackPartition is what the artifacts of one group are checked against.
type slackPartition struct {
	s      *Session
	fsys   filesys.FileSystem
	part   volume.Partition
	k      slackGroup
	free   *freeMap // built on first use
	freeEr error
	plans  map[bool]*slackReplan // by include_zero
}

// slackReplan is one re-run of planSlack (or the reason it did not run).
type slackReplan struct {
	plan *slackPlan
	err  error
}

// group rechecks the artifacts that name one (parent, partition, offset, filesystem). It returns false when
// the context ended; the findings made so far are kept.
func (v *slackVerifier) group(s *Session, openErr error, k slackGroup, g []evidence.ManifestRecord) bool {
	skip := func(format string, args ...any) bool {
		v.problem(g[0], format+" (%d artifact(s) not rechecked)", append(args, len(g))...)
		v.add(int64(len(g)))
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
	if !filesys.ReportsSlack(fsys) {
		return skip("the filesystem reader of partition %d cannot state its allocation (no slack can be re-derived)", k.partition)
	}
	p := &slackPartition{s: s, fsys: fsys, part: part, k: k, plans: map[bool]*slackReplan{}}
	// A complete file and volume artifact of one include_zero setting share one re-plan.
	need := map[bool][2]bool{}
	for _, rec := range g {
		if iz, ok := includeZeroOf(rec); ok && !rec.Incomplete {
			n := need[iz]
			if rec.Source.Derived.Recovery.Method == evidence.MethodSlackFile {
				n[0] = true
			} else {
				n[1] = true
			}
			need[iz] = n
		}
	}
	for _, rec := range g {
		if slackArtifactHook != nil {
			slackArtifactHook(rec.ID)
		}
		if v.ctx.Err() != nil {
			return false
		}
		if !v.one(p, rec, need) {
			return false
		}
		v.add(1)
	}
	return true
}

func includeZeroOf(rec evidence.ManifestRecord) (value, ok bool) {
	s, has := rec.Source.Derived.Recovery.Params["include_zero"]
	if !has {
		return false, false
	}
	b, err := strconv.ParseBool(s)
	return b, err == nil
}

// replan returns the re-run of the slack plan for one include_zero setting, planning once.
func (p *slackPartition) replan(ctx context.Context, includeZero bool, need [2]bool) *slackReplan {
	if r, ok := p.plans[includeZero]; ok {
		return r
	}
	b, err := newBudget(evidence.MaxRecoveredRuns, math.MaxInt64)
	r := &slackReplan{err: err}
	if err == nil {
		r.plan, r.err = p.s.planSlack(ctx, SlackOptions{Partition: p.k.partition, Files: need[0], Volume: need[1], IncludeZero: includeZero}, p.fsys, p.part, b)
	}
	p.plans[includeZero] = r
	return r
}

// freeMap is built exactly as the export builds it.
func (p *slackPartition) freeMap() (*freeMap, error) {
	if p.free == nil && p.freeEr == nil {
		runs, _, err := p.s.unallocatedRunsNote(p.fsys, p.part, false)
		if err != nil {
			p.freeEr = err
		} else {
			p.free = newFreeMap(runs, len(p.fsys.Info().Warnings) > 0)
		}
	}
	return p.free, p.freeEr
}

// slackRange is an exported segment in the parent image, for the overlap check.
type slackRange struct {
	off, end int64
	idx      int
	omitted  bool // an omitted uniform range, not an exported segment
}

func (r slackRange) String() string {
	if r.omitted {
		return fmt.Sprintf("omitted range %d", r.idx)
	}
	return fmt.Sprintf("segment %d", r.idx)
}

// one checks one artifact. It returns false when the context ended.
func (v *slackVerifier) one(p *slackPartition, rec evidence.ManifestRecord, need map[bool][2]bool) bool {
	d := rec.Source.Derived
	rv := d.Recovery
	file := rv.Method == evidence.MethodSlackFile
	before := v.raised
	if d.RunsArtifact == "" {
		v.problem(rec, "the artifact records no run map")
		return true
	}
	includeZero, izOK := includeZeroOf(rec)
	if !izOK {
		v.problem(rec, "the parameter include_zero is missing or not a boolean")
	}
	side, _, err := v.c.OpenArtifactIn(v.ix, d.RunsArtifact)
	if err != nil {
		v.problem(rec, "the run map cannot be opened: %v", err)
		return true
	}
	defer func() { _ = side.Close() }()
	bin, _, err := v.c.OpenArtifactIn(v.ix, rec.ID)
	if err != nil {
		v.problem(rec, "the artifact cannot be opened: %v", err)
		return true
	}
	defer func() { _ = bin.Close() }()

	complete := !rec.Incomplete
	var re *slackReplan
	var want []SlackSegment
	if complete && izOK {
		re = p.replan(v.ctx, includeZero, need[includeZero])
		switch {
		case v.ctx.Err() != nil:
			return false
		case re.err != nil:
			v.problem(rec, "the slack plan cannot be re-run: %v", re.err)
			re = nil
		case file && re.plan.fileIncomplete():
			v.problem(rec, "the artifact is complete, but the re-plan of the file slack is incomplete (%s)", re.plan.fileErrText())
			re = nil
		case !file && re.plan.volumeIncomplete():
			v.problem(rec, "the artifact is complete, but the re-plan of the volume slack is incomplete (%s)", re.plan.volumeErrText())
			re = nil
		case file:
			want = re.plan.file
		default:
			want = re.plan.vol
		}
	}

	c := &slackArtifactCheck{v: v, p: p, rec: rec, file: file, includeZero: includeZero, izOK: izOK, complete: complete, want: want, replanned: re != nil, bin: bin}
	var pending *evidence.SlackMapLine
	flush := func(last bool) {
		if pending != nil {
			c.line(*pending, last)
			pending = nil
		}
	}
	err = evidence.ReadSlackRunMap(side, rv.Method, func(l evidence.SlackMapLine) error {
		if err := v.ctx.Err(); err != nil {
			return err
		}
		if !l.Exported {
			c.omitted(l)
			return nil
		}
		flush(false)
		cp := l
		pending = &cp
		return nil
	})
	if err != nil {
		if v.ctx.Err() != nil {
			return false
		}
		v.problem(rec, "the run map is unreadable: %v", err)
		return true
	}
	flush(true)
	if v.ctx.Err() != nil {
		return false
	}
	c.finish(re)
	if v.ctx.Err() != nil {
		return false
	}
	if v.raised == before {
		v.rep.SlackRechecked++
	}
	return true
}

// slackArtifactCheck is the state of one artifact's check.
type slackArtifactCheck struct {
	v           *slackVerifier
	p           *slackPartition
	rec         evidence.ManifestRecord
	file        bool
	includeZero bool
	izOK        bool
	complete    bool
	want        []SlackSegment // the re-plan's exported segments (a complete artifact)
	replanned   bool
	bin         io.ReaderAt

	n        int // exported lines seen
	nOmitted int // omitted lines seen
	diffDone bool
	ranges   []slackRange
	free     *freeMap
	freeDone bool
	last     evidence.SlackMapLine
}

func (c *slackArtifactCheck) problem(format string, args ...any) { c.v.problem(c.rec, format, args...) }

// omitted checks an omitted range of an incomplete artifact: uniform in the image.
func (c *slackArtifactCheck) omitted(l evidence.SlackMapLine) {
	if c.complete {
		c.problem("a complete artifact lists an omitted range (%d+%d)", l.ImageOffset, l.Length)
		return
	}
	img := c.p.s.Image.Size()
	if end, ok := filesys.AddOK(l.ImageOffset, l.Length); !ok || end > img {
		c.problem("omitted range %d+%d lies outside the %d-byte image", l.ImageOffset, l.Length, img)
		return
	}
	// The scan waits for finish: only ranges that are disjoint from each other and from the exported segments
	// are read, so the bytes scanned never exceed the image size however many lines a forged map repeats.
	end, _ := filesys.AddOK(l.ImageOffset, l.Length)
	c.ranges = append(c.ranges, slackRange{off: l.ImageOffset, end: end, idx: c.nOmitted, omitted: true})
	c.nOmitted++
}

// scanOmitted reads the omitted ranges (disjoint, see finish) and requires each to be uniform in the image.
func (c *slackArtifactCheck) scanOmitted() {
	for _, r := range c.ranges {
		if !r.omitted {
			continue
		}
		if c.v.ctx.Err() != nil {
			return
		}
		uniform, _, err := uniformScanBuf(c.v.ctx, c.p.s.Image, []evidence.Run{{Offset: r.off, Length: r.end - r.off}}, c.v.buf)
		switch {
		case err != nil:
			if c.v.ctx.Err() == nil {
				c.problem("omitted range %d (%d+%d) cannot be read from the image: %v", r.idx, r.off, r.end-r.off, err)
			}
		case !uniform:
			c.problem("omitted range %d (%d+%d) is not uniform in the image", r.idx, r.off, r.end-r.off)
		}
	}
}

// line checks one exported line; last marks the final exported line of the map.
func (c *slackArtifactCheck) line(l evidence.SlackMapLine, last bool) {
	i := c.n
	c.n++
	c.last = l
	end, ok := filesys.AddOK(l.ImageOffset, l.Length)
	if !ok {
		c.problem("segment %d: %d+%d overflows", i, l.ImageOffset, l.Length)
		return
	}
	c.ranges = append(c.ranges, slackRange{off: l.ImageOffset, end: end, idx: i})

	// the bytes of the artifact decide the uniform flag
	if abEnd, ok := filesys.AddOK(l.Offset, l.Length); !ok || abEnd > c.rec.Size {
		c.problem("segment %d lies beyond the %d bytes of the artifact (offset %d, length %d)", i, c.rec.Size, l.Offset, l.Length)
	} else {
		uniform, _, err := uniformScanBuf(c.v.ctx, c.bin, []evidence.Run{{Offset: l.Offset, Length: l.Length}}, c.v.buf)
		switch {
		case err != nil:
			if c.v.ctx.Err() == nil {
				c.problem("segment %d cannot be read from the artifact: %v", i, err)
			}
		case uniform != l.Uniform:
			c.problem("segment %d records uniform %v, but its bytes in the artifact are %s", i, l.Uniform, map[bool]string{true: "uniform", false: "not uniform"}[uniform])
		}
	}
	if l.Uniform && c.izOK && !c.includeZero {
		c.problem("segment %d is uniform but include_zero is not set: uniform segments are never exported unless asked", i)
	}

	if c.complete {
		c.compare(i, l)
		return
	}
	if c.file {
		c.fileSegment(i, l, last)
	} else {
		c.volumeSegment(i, l)
	}
}

// compare is the complete artifact's check of segment i against the re-plan.
func (c *slackArtifactCheck) compare(i int, l evidence.SlackMapLine) {
	if !c.replanned || c.diffDone {
		return
	}
	if i >= len(c.want) {
		c.diffDone = true
		c.problem("segment %d is not in the re-plan (the re-plan has %d exported segment(s))", i, len(c.want))
		return
	}
	w := c.want[i]
	var diff string
	switch {
	case l.ImageOffset != w.ImageOffset:
		diff = fmt.Sprintf("image_offset is %d, the re-plan gives %d", l.ImageOffset, w.ImageOffset)
	case l.Length != w.Length:
		diff = fmt.Sprintf("length is %d, the re-plan gives %d", l.Length, w.Length)
	case l.Uniform != w.Uniform:
		diff = fmt.Sprintf("uniform is %v, the re-plan gives %v", l.Uniform, w.Uniform)
	case c.file && l.FSID != w.FSID:
		diff = fmt.Sprintf("fs_id is %q, the re-plan gives %q", l.FSID, w.FSID)
	case c.file && l.FSPath != w.FSPath:
		diff = fmt.Sprintf("fs_path is %q, the re-plan gives %q", l.FSPath, w.FSPath)
	case c.file && l.PathEscaped != w.PathEscaped:
		diff = fmt.Sprintf("path_escaped is %v, the re-plan gives %v", l.PathEscaped, w.PathEscaped)
	case c.file && l.FileSize != w.FileSize:
		diff = fmt.Sprintf("file_size is %d, the re-plan gives %d", l.FileSize, w.FileSize)
	}
	if diff != "" {
		c.diffDone = true
		c.problem("segment %d differs from the re-plan: %s", i, diff)
	}
}

func (c *slackArtifactCheck) freeMap() *freeMap {
	if !c.freeDone {
		c.freeDone = true
		m, err := c.p.freeMap()
		if err != nil {
			c.problem("the unallocated space of partition %d cannot be listed: %v", c.p.k.partition, err)
		}
		c.free = m
	}
	return c.free
}

// fileSegment is the per-segment check of an incomplete file artifact: the segment is the tail slack of the
// file its fs_id names now, and that unit is not free. The last line of a narrowed artifact may be a strict
// prefix of the line it was cut from.
func (c *slackArtifactCheck) fileSegment(i int, l evidence.SlackMapLine, last bool) {
	f, err := c.p.fsys.Open(filesys.Entry{ID: l.FSID})
	if err != nil {
		c.problem("segment %d (fs_id %q): the file cannot be opened: %v", i, l.FSID, err)
		return
	}
	c.entryType(i, l)
	if f.Size() != l.FileSize {
		c.problem("segment %d (fs_id %q): the file is %d bytes now, the segment records %d", i, l.FSID, f.Size(), l.FileSize)
	}
	if filesys.FileEncrypted(f) {
		c.problem("segment %d (fs_id %q): the file is encrypted: its tail is not residue", i, l.FSID)
	}
	info := c.p.fsys.Info()
	alloc, _ := filesys.AllocatedRunsOf(f)
	run, outcome, why := filesys.TailSlack(f.Size(), f.Runs(), alloc, int64(info.BlockSize), info.Size)
	if outcome != filesys.SlackFound {
		if why != "" {
			why = ": " + why
		}
		c.problem("segment %d (fs_id %q): the file has no tail slack now (%s%s)", i, l.FSID, outcome, why)
	} else {
		off, ok := filesys.AddOK(run.Offset, c.p.part.Start)
		prefixOK := last && c.rec.Source.Derived.Recovery.DeclaredRunsArtifact != "" && l.Length < run.Length
		if !ok || l.ImageOffset != off || (l.Length != run.Length && !prefixOK) {
			c.problem("segment %d (fs_id %q): %d+%d is not the tail slack of the file, which is %d+%d", i, l.FSID, l.ImageOffset, l.Length, off, run.Length)
		}
	}
	if m := c.freeMap(); m != nil && m.touches(l.ImageOffset, l.Length) {
		c.problem("segment %d (fs_id %q): %d+%d lies in space the filesystem reports unallocated", i, l.FSID, l.ImageOffset, l.Length)
	}
}

// volumeSegment is the per-segment check of an incomplete volume artifact: past the end of the filesystem,
// inside the partition.
func (c *slackArtifactCheck) volumeSegment(i int, l evidence.SlackMapLine) {
	size := c.p.fsys.Info().Size
	lo, ok1 := filesys.AddOK(c.p.part.Start, size)
	hi, ok2 := filesys.AddOK(c.p.part.Start, c.p.part.Length)
	end, ok3 := filesys.AddOK(l.ImageOffset, l.Length)
	switch {
	case size <= 0 || !ok1 || !ok2 || !ok3:
		c.problem("volume segment %d (%d+%d): the bounds of the volume slack cannot be decided", i, l.ImageOffset, l.Length)
	case l.ImageOffset < lo:
		c.problem("volume segment %d (%d+%d) lies inside the filesystem (volume slack starts at %d)", i, l.ImageOffset, l.Length, lo)
	case end > hi:
		c.problem("volume segment %d (%d+%d) lies past the end of the partition (%d)", i, l.ImageOffset, l.Length, hi)
	}
}

// finish runs the checks that need the whole map: the overlap check, the counters and the narrowed last line.
func (c *slackArtifactCheck) finish(re *slackReplan) {
	sort.Slice(c.ranges, func(a, b int) bool {
		if c.ranges[a].off != c.ranges[b].off {
			return c.ranges[a].off < c.ranges[b].off
		}
		return c.ranges[a].idx < c.ranges[b].idx
	})
	var maxEnd int64 = -1
	var maxR slackRange
	overlap := false
	for _, r := range c.ranges {
		if r.off < maxEnd {
			overlap = true
			a, b := maxR, r
			if a.omitted == b.omitted && a.idx > b.idx {
				a, b = b, a
			}
			if a.omitted || b.omitted {
				c.problem("%s and %s overlap in the image (omitted ranges are disjoint from each other and from the exported segments)", a, b)
			} else {
				c.problem("segments %d and %d overlap in the image", a.idx, b.idx)
			}
			break
		}
		if r.end > maxEnd {
			maxEnd, maxR = r.end, r
		}
	}
	if !c.complete && !overlap {
		c.scanOmitted()
	}
	if c.complete {
		if re != nil {
			if c.n < len(c.want) {
				c.problem("the artifact lists %d exported segment(s), the re-plan has %d", c.n, len(c.want))
			}
			c.compareParams(re.plan)
		}
		return
	}
	c.declaredLine()
}

// compareParams requires the counters of the artifact's Params to equal those of the re-plan.
func (c *slackArtifactCheck) compareParams(plan *slackPlan) {
	got := c.rec.Source.Derived.Recovery.Params
	want := slackParams(plan, c.file, c.includeZero)
	keys := make([]string, 0, len(want)+len(got))
	for k := range want {
		keys = append(keys, k)
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		g, gok := got[k]
		w, wok := want[k]
		switch {
		case gok && wok && g == w:
		case gok && wok:
			c.problem("parameter %q is %q, the re-plan gives %q", k, g, w)
			return
		case gok:
			c.problem("parameter %q (%q) is not one the re-plan gives", k, g)
			return
		default:
			c.problem("parameter %q is missing, the re-plan gives %q", k, w)
			return
		}
	}
}

var errSlackStop = errors.New("stop")

// declaredLine checks the last captured line of a narrowed artifact against the line of the declared full
// map it was cut from: the same segment, never longer.
func (c *slackArtifactCheck) declaredLine() {
	rv := c.rec.Source.Derived.Recovery
	if rv.DeclaredRunsArtifact == "" || c.n == 0 {
		return
	}
	f, _, err := c.v.c.OpenArtifactIn(c.v.ix, rv.DeclaredRunsArtifact)
	if err != nil {
		c.problem("the declared run map cannot be opened: %v", err)
		return
	}
	defer func() { _ = f.Close() }()
	var decl evidence.SlackMapLine
	k, found := 0, false
	err = evidence.ReadSlackRunMap(f, rv.Method, func(l evidence.SlackMapLine) error {
		if err := c.v.ctx.Err(); err != nil {
			return err
		}
		if !l.Exported {
			return nil
		}
		if k == c.n-1 {
			decl, found = l, true
			return errSlackStop
		}
		k++
		return nil
	})
	switch {
	case c.v.ctx.Err() != nil:
		return
	case err != nil && !errors.Is(err, errSlackStop):
		c.problem("the declared run map is unreadable: %v", err)
		return
	case !found:
		c.problem("the declared run map has %d exported line(s), the artifact captured %d", k, c.n)
		return
	}
	l := c.last
	switch {
	case l.ImageOffset != decl.ImageOffset || l.FSID != decl.FSID || l.FSPath != decl.FSPath || l.PathEscaped != decl.PathEscaped || l.FileSize != decl.FileSize:
		c.problem("the last captured line (%d+%d) is not the line it was cut from (declared %d+%d)", l.ImageOffset, l.Length, decl.ImageOffset, decl.Length)
	case l.Length > decl.Length:
		c.problem("the last captured line is longer (%d bytes) than the declared line it was cut from (%d bytes)", l.Length, decl.Length)
	}
}

// entryType requires the recorded path to name, now, the regular file the fs_id names: Open gives no node
// type, so a fs_id that has become a symlink would otherwise be judged by its runs alone. A path the walk
// had to escape cannot be looked up (limitations); its fs_id is still opened.
func (c *slackArtifactCheck) entryType(i int, l evidence.SlackMapLine) {
	if l.PathEscaped {
		return
	}
	e, err := c.p.fsys.Lookup(l.FSPath)
	switch {
	case err != nil:
		c.problem("segment %d (fs_id %q): the path %q cannot be resolved: %v", i, l.FSID, l.FSPath, err)
	case e.Type != filesys.TypeFile:
		c.problem("segment %d (fs_id %q): the path %q names a %v, not a regular file", i, l.FSID, l.FSPath, e.Type)
	case e.ID != l.FSID:
		c.problem("segment %d (fs_id %q): the path %q now names %q", i, l.FSID, l.FSPath, e.ID)
	}
}
