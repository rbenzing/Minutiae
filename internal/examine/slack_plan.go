package examine

import (
	"context"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

// The slack planner (plan 3F, Task 8): it walks the live tree of a partition twice, derives the slack of
// every live regular file from its runs and whole allocation units (filesys.TailSlack), reads only the tail
// bytes it must classify, and plans the volume slack past the end of the filesystem. It is read-only: it
// writes nothing to the case and appends nothing to the audit log; ExportSlack audits and writes the plan.
//
// planSlack is deterministic (it depends on the image, the reader and SlackOptions only), because case
// verify re-runs it for a complete artifact.

// SlackOptions select what ExportSlack writes.
type SlackOptions struct {
	Partition     int  // -1 = auto
	Files, Volume bool // what to export; both false = both (the CLI refuses both true)
	IncludeZero   bool
	MaxBytes      int64 // 0 = DefaultMaxBytes; negative = ErrInvalidLimit
	Progress      func(done, total int64)
}

// SlackSegment is one planned range of slack.
type SlackSegment struct {
	ImageOffset, Length int64
	FSPath, FSID        string // file segments; FSPath is the escaped label when PathEscaped (3F-R28)
	PathEscaped         bool
	FileSize            int64
	Uniform             bool
	Fill                byte // the fill byte of a uniform segment (3F-R20)
}

// SlackVolumeChunk is the granularity of the uniform test of volume slack.
const SlackVolumeChunk = 4096

// Outcomes the planner adds to the filesys.SlackOutcome names (3F-R2).
const (
	slackBeyondImage         = "beyond-image"
	slackDuplicateID         = "duplicate-id"
	slackEncrypted           = "encrypted"
	slackFreeSpace           = "free-space"
	slackLimit               = "limit"
	slackNoAllocation        = "no-allocation"
	slackOpenError           = "open-error"
	slackReadError           = "read-error"
	slackSharedUnit          = "shared-unit"
	slackSizeBeyondPartition = "size-beyond-partition"
	slackSizeUnknown         = "size-unknown"
)

// Seams and caps (tests lower them; the real values are far too large to build in a test).
var (
	slackSegmentCap   = evidence.MaxRecoveredRuns
	slackOmittedCap   = evidence.MaxRecoveredRuns // omitted uniform ranges listed per map (the reader's cap)
	slackPathBytesCap = 64 << 20
	slackWarnCap      = 1000
	// slackReadObserver, when set, sees every read of image bytes the planner makes (offset, length).
	slackReadObserver func(off int64, n int)
)

// slackPlan is what ExportSlack would write.
type slackPlan struct {
	part         volume.Partition
	fsType       string
	unit, fsSize int64
	file, vol    []SlackSegment // exported, walk order / image order

	files            int            // live regular files visited
	skipped          map[string]int // outcome -> files (3F-R2 names, plus "limit"); volume-level counts are included, see volCounted
	volCounted       int            // how many of skipped's counts belong to the volume part, not to files
	uniformSegs      int
	uniformBytes     int64 // file segments counted, not exported
	volUniformChunks int
	volUniformBytes  int64
	walkErrors       int
	firstWalkError   string
	limit            string         // "", "max-bytes", "max-segments", "max-path-bytes", "max-scan-bytes"
	omitted          []SlackSegment // uniform file ranges not exported, kept only when the file part is incomplete (3F-R19)
	volOmitted       []SlackSegment // the same for the volume part
	omittedLost      int            // omitted ranges not listed because the list reached its cap (the counters stay exact)
	volOmittedLost   int
	volStop          string // why the volume part was not read to the end ("" = it was)
	warnings         []Warning
	fsNote           string // the unallocatedRunsNote text, audited by the export
}

// fileIncomplete reports whether the file part of the plan does not cover every file (a walk error or a limit).
func (p *slackPlan) fileIncomplete() bool { return p.walkErrors > 0 || p.limit != "" }

// volumeIncomplete reports whether the volume part was not planned to the end.
func (p *slackPlan) volumeIncomplete() bool { return p.limit != "" || p.volStop != "" }

// fileOutcomes returns the number of files that ended in a counted outcome: it must equal files.
func (p *slackPlan) fileOutcomes() int {
	n := len(p.file) + p.uniformSegs - p.volCounted
	for _, c := range p.skipped {
		n += c
	}
	return n
}

// slackScanBudget is the bound on the bytes the planner reads for classification: 4 x maxBytes, lowered to the
// session's scan limit when it has one (3F-R18, the recover precedent).
func slackScanBudget(s *Session, maxBytes int64) int64 {
	b, ok := filesys.MulOK(maxBytes, 4)
	if !ok {
		b = math.MaxInt64
	}
	if lim, _ := s.ScanLimit(); lim > 0 && lim < b {
		return lim
	}
	return b
}

// touches reports whether any byte of [off, off+length) is free. A range it cannot decide (negative,
// overflowing) counts as touching, so it is never exported.
func (m *freeMap) touches(off, length int64) bool {
	if length <= 0 {
		return false
	}
	end, ok := filesys.AddOK(off, length)
	if off < 0 || !ok {
		return true
	}
	i := sort.Search(len(m.free), func(i int) bool { return m.free[i].Offset+m.free[i].Length > off })
	return i < len(m.free) && m.free[i].Offset < end
}

// escapePathLabel makes a walk path safe for the slack map (3F-R28): a valid UTF-8 path without NUL is its
// own label; otherwise each byte of an invalid sequence and each NUL becomes \xHH and a backslash becomes \\.
func escapePathLabel(p string) (label string, escaped bool) {
	if utf8.ValidString(p) && !strings.ContainsRune(p, 0) {
		return p, false
	}
	var sb strings.Builder
	for i := 0; i < len(p); {
		r, n := utf8.DecodeRuneInString(p[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			fmt.Fprintf(&sb, `\x%02X`, p[i])
		case r == 0:
			sb.WriteString(`\x00`)
		case r == '\\':
			sb.WriteString(`\\`)
		default:
			sb.WriteString(p[i : i+n])
		}
		i += max(n, 1)
	}
	return sb.String(), true
}

// observedReader reports every read to slackReadObserver.
type observedReader struct{ r io.ReaderAt }

func (o observedReader) ReadAt(p []byte, off int64) (int, error) {
	if slackReadObserver != nil {
		slackReadObserver(off, len(p))
	}
	return o.r.ReadAt(p, off)
}

// slackCand is a file whose tail may be slack; no byte of it has been read yet.
type slackCand struct {
	path, id    string
	size        int64
	off, length int64 // the tail in the image
	unit        int64 // image offset of the allocation unit holding the tail
	dropped     bool  // shared with another file's units
}

type slackPlanner struct {
	s        *Session
	ctx      context.Context
	o        SlackOptions
	fsys     filesys.FileSystem
	part     volume.Partition
	b        *budget
	p        *slackPlan
	rd       observedReader
	imgSize  int64
	scanLeft int64
	free     *freeMap

	warnN map[string]int
}

// planSlack plans the file and/or volume slack of part. It appends nothing to the audit log and writes
// nothing. Only a context error, a failure to list the free space and a bad budget end it with an error.
func (s *Session) planSlack(ctx context.Context, o SlackOptions, fsys filesys.FileSystem, part volume.Partition, b *budget) (*slackPlan, error) {
	info := fsys.Info()
	pl := &slackPlanner{
		s: s, ctx: ctx, o: o, fsys: fsys, part: part, b: b,
		p: &slackPlan{
			part: part, fsType: info.Type, unit: int64(info.BlockSize), fsSize: info.Size,
			skipped: map[string]int{},
		},
		rd:       observedReader{r: s.Image},
		imgSize:  s.Image.Size(),
		scanLeft: slackScanBudget(s, b.maxBytes),
		warnN:    map[string]int{},
	}
	doFiles := o.Files || !o.Volume
	doVol := o.Volume || !o.Files
	if doFiles {
		if err := pl.planFiles(); err != nil {
			return nil, err
		}
	}
	if doVol {
		if err := pl.planVolume(); err != nil {
			return nil, err
		}
	}
	if !pl.p.fileIncomplete() {
		pl.p.omitted, pl.p.omittedLost = nil, 0
	}
	if !pl.p.volumeIncomplete() {
		pl.p.volOmitted, pl.p.volOmittedLost = nil, 0
	}
	pl.warnTotals()
	return pl.p, nil
}

// warn records an anomaly warning; at most slackWarnCap per reason are kept, the rest are summarised.
func (pl *slackPlanner) warn(reason, path, detail string) {
	pl.warnN[reason]++
	if pl.warnN[reason] <= slackWarnCap {
		pl.p.warnings = append(pl.p.warnings, Warning{Path: path, Reason: reason + ": " + detail})
	}
}

// warnTotals adds one count note for every reason that passed its cap.
func (pl *slackPlanner) warnTotals() {
	reasons := make([]string, 0, len(pl.warnN))
	for r, n := range pl.warnN {
		if n > slackWarnCap {
			reasons = append(reasons, r)
		}
	}
	slices.Sort(reasons)
	for _, r := range reasons {
		pl.p.warnings = append(pl.p.warnings, Warning{Reason: fmt.Sprintf("%s: %d further warnings not listed one by one", r, pl.warnN[r]-slackWarnCap)})
	}
}

// setLimit records the first limit met.
func (pl *slackPlanner) setLimit(which, detail string) {
	if pl.p.limit == "" {
		pl.p.limit = which
		pl.warn("limit", "", which+": "+detail)
	}
}

// skip counts one file in an outcome.
func (pl *slackPlanner) skip(reason string) { pl.p.skipped[reason]++ }

// volSkip counts a volume-level outcome.
func (pl *slackPlanner) volSkip(reason string) {
	pl.p.skipped[reason]++
	pl.p.volCounted++
}

func walkReasonText(err error) string { return walkReason(err) + ": " + err.Error() }

// planFiles is pass 1 (candidates), the shared-unit passes and the tail reads.
func (pl *slackPlanner) planFiles() error {
	runs, note, err := pl.s.unallocatedRunsNote(pl.fsys, pl.part, false)
	if err != nil {
		return err
	}
	pl.p.fsNote = note
	pl.free = newFreeMap(runs, false)

	cands, err := pl.collect()
	if err != nil {
		return err
	}
	pl.groupUnits(cands)
	if err := pl.crossCheck(cands); err != nil {
		return err
	}
	return pl.readTails(cands)
}

// collect is pass 1: walk the live tree, decide every file that cannot give a segment, and keep the rest as
// candidates (no content read).
func (pl *slackPlanner) collect() ([]*slackCand, error) {
	var cands []*slackCand
	seen := map[string]struct{}{}
	err := filesys.Walk(pl.fsys, pl.fsys.Root(), "/", func(wp string, we filesys.Entry, werr error) error {
		if err := pl.ctx.Err(); err != nil {
			return err
		}
		if werr != nil {
			pl.p.walkErrors++
			if pl.p.walkErrors == 1 {
				pl.p.firstWalkError = wp + ": " + walkReasonText(werr)
			}
			pl.warn("walk-error", wp, walkReasonText(werr))
			return nil
		}
		if filesys.IsSnapshotsDir(we) {
			return filesys.SkipDir
		}
		if we.Deleted || we.Type != filesys.TypeFile {
			return nil
		}
		pl.p.files++
		if pl.p.limit != "" {
			pl.skip(slackLimit)
			return nil
		}
		c, outcome, detail := pl.examine(wp, we, seen)
		if outcome != "" {
			pl.skip(outcome)
			if detail != "" {
				pl.warn(outcome, wp, detail)
			}
			return nil
		}
		if len(cands) >= slackSegmentCap {
			pl.setLimit("max-segments", fmt.Sprintf("more than %d files with slack", slackSegmentCap))
			pl.skip(slackLimit)
			return nil
		}
		cands = append(cands, c)
		return nil
	})
	return cands, err
}

// examine decides one file: it returns a candidate, or the outcome (and a warning text) that ends it.
func (pl *slackPlanner) examine(wp string, we filesys.Entry, seen map[string]struct{}) (*slackCand, string, string) {
	if we.Encrypted {
		return nil, slackEncrypted, ""
	}
	if _, dup := seen[we.ID]; dup {
		return nil, slackDuplicateID, ""
	}
	seen[we.ID] = struct{}{}
	f, err := pl.fsys.Open(we)
	if err != nil {
		return nil, slackOpenError, err.Error()
	}
	if filesys.FileEncrypted(f) {
		return nil, slackEncrypted, ""
	}
	alloc, hasAlloc := filesys.AllocatedRunsOf(f)
	run, outcome, why := filesys.TailSlack(f.Size(), f.Runs(), alloc, pl.p.unit, pl.p.fsSize)
	switch outcome {
	case filesys.SlackFound:
	case filesys.SlackAllocMismatch:
		if !hasAlloc {
			return nil, slackNoAllocation, "the reader does not report the allocation of this file"
		}
		return nil, string(outcome), why
	case filesys.SlackInvalidRuns:
		return nil, string(outcome), why
	default:
		return nil, string(outcome), ""
	}
	end, ok1 := filesys.AddOK(run.Offset, run.Length)
	off, ok2 := filesys.AddOK(run.Offset, pl.part.Start)
	imgEnd, ok3 := filesys.AddOK(off, run.Length)
	if !ok1 || !ok2 || !ok3 || end > pl.part.Length || imgEnd > pl.imgSize {
		return nil, slackBeyondImage, "the slack lies outside the partition or the image"
	}
	if pl.free.touches(off, run.Length) {
		return nil, slackFreeSpace, "the allocation unit of this file's last byte is marked free"
	}
	return &slackCand{path: wp, id: we.ID, size: f.Size(), off: off, length: run.Length, unit: off + run.Length - pl.p.unit}, "", ""
}

// groupUnits marks every candidate whose allocation unit is claimed by two or more IDs (3F-R18). A shared unit
// is never read.
func (pl *slackPlanner) groupUnits(cands []*slackCand) {
	first := make(map[int64]*slackCand, len(cands))
	for _, c := range cands {
		if o, ok := first[c.unit]; ok {
			o.dropped, c.dropped = true, true
			continue
		}
		first[c.unit] = c
	}
}

// crossCheck is pass 2 (3F-R17): no candidate may overlap another candidate or any other live file's whole
// units. Every live regular file is opened again (no content read) and its units looked up in the sorted
// candidates, so memory stays bounded whatever the file count.
func (pl *slackPlanner) crossCheck(cands []*slackCand) error {
	var alive []*slackCand
	for _, c := range cands {
		if !c.dropped {
			alive = append(alive, c)
		}
	}
	if len(alive) == 0 {
		return nil
	}
	slices.SortStableFunc(alive, func(a, b *slackCand) int {
		switch {
		case a.off < b.off:
			return -1
		case a.off > b.off:
			return 1
		}
		return 0
	})
	// Candidate against candidate: an overlap drops both, wherever the other one sits in the order.
	var stack []*slackCand
	var maxEnd int64 = -1
	for _, c := range alive {
		if c.off < maxEnd {
			c.dropped = true
			if n := len(stack); n > 0 && stack[n-1].off+stack[n-1].length > c.off {
				stack[n-1].dropped = true
				stack = stack[:n-1]
			}
		} else {
			stack = append(stack, c)
		}
		maxEnd = max(maxEnd, c.off+c.length)
	}
	segs := alive[:0:0]
	for _, c := range alive {
		if !c.dropped {
			segs = append(segs, c)
		}
	}
	if len(segs) == 0 {
		return nil
	}
	next := make([]int32, len(segs)+1)
	for i := range next {
		next[i] = int32(i)
	}
	find := func(i int) int {
		r := i
		for int(next[r]) != r {
			r = int(next[r])
		}
		for int(next[i]) != r {
			i, next[i] = int(next[i]), int32(r)
		}
		return r
	}
	drop := func(i int) {
		segs[i].dropped = true
		next[i] = int32(i + 1)
	}
	check := func(id string, off, length int64) {
		if length <= 0 || off < 0 {
			return
		}
		end, ok := filesys.AddOK(off, length)
		if !ok {
			return
		}
		lo := sort.Search(len(segs), func(i int) bool { return segs[i].off+segs[i].length > off })
		for j := find(lo); j < len(segs) && segs[j].off < end; j = find(j + 1) {
			if segs[j].id != id {
				drop(j)
			}
		}
	}
	return filesys.Walk(pl.fsys, pl.fsys.Root(), "/", func(wp string, we filesys.Entry, werr error) error {
		if err := pl.ctx.Err(); err != nil {
			return err
		}
		if werr != nil {
			return nil // counted in pass 1
		}
		if filesys.IsSnapshotsDir(we) {
			return filesys.SkipDir
		}
		if we.Deleted || we.Type != filesys.TypeFile {
			return nil
		}
		f, err := pl.fsys.Open(we)
		if err != nil {
			pl.warn("shared-unit-check", wp, "could not be opened for the shared-unit check: "+err.Error())
			return nil
		}
		runs := f.Runs()
		alloc, _ := filesys.AllocatedRunsOf(f)
		for _, group := range [2][]filesys.Run{alloc, filesys.UnitRuns(runs, pl.p.unit)} {
			for _, r := range group {
				if r.Offset < 0 || r.Length <= 0 {
					continue
				}
				off, ok := filesys.AddOK(r.Offset, pl.part.Start)
				if !ok {
					continue
				}
				check(we.ID, off, r.Length)
			}
		}
		return nil
	})
}

// readTails classifies the surviving candidates in walk order under the caps (3F-R10, 3F-R18).
func (pl *slackPlanner) readTails(cands []*slackCand) error {
	var pathBytes int
	stopped := false // a limit met while reading tails (a full candidate list alone does not stop the reads)
	for _, c := range cands {
		if c.dropped {
			pl.skip(slackSharedUnit)
			pl.warn(slackSharedUnit, c.path, "its last allocation unit is also allocated to another file")
			continue
		}
		if stopped {
			pl.skip(slackLimit)
			continue
		}
		if c.length > pl.scanLeft {
			stopped = true
			pl.setLimit("max-scan-bytes", "the read budget for tail bytes is spent")
			pl.skip(slackLimit)
			continue
		}
		uniform, fill, err := uniformScan(pl.ctx, pl.rd, []evidence.Run{{Offset: c.off, Length: c.length}})
		if err != nil {
			if cerr := pl.ctx.Err(); cerr != nil {
				return cerr
			}
			pl.skip(slackReadError)
			pl.warn(slackReadError, c.path, err.Error())
			continue
		}
		pl.scanLeft -= c.length
		label, escaped := escapePathLabel(c.path)
		seg := SlackSegment{
			ImageOffset: c.off, Length: c.length, FSPath: label, FSID: c.id, PathEscaped: escaped,
			FileSize: c.size, Uniform: uniform, Fill: fill,
		}
		if !uniform {
			seg.Fill = 0
		}
		if uniform && !pl.o.IncludeZero {
			pl.p.uniformSegs++
			pl.p.uniformBytes += c.length
			if len(pl.p.omitted) < slackOmittedCap {
				pl.p.omitted = append(pl.p.omitted, seg)
			} else {
				pl.p.omittedLost++
			}
			continue
		}
		stopped = true
		switch {
		case len(pl.p.file) >= slackSegmentCap:
			pl.setLimit("max-segments", fmt.Sprintf("more than %d file segments", slackSegmentCap))
		case pathBytes+len(label) > slackPathBytesCap:
			pl.setLimit("max-path-bytes", fmt.Sprintf("more than %d bytes of exported path text", slackPathBytesCap))
		default:
			switch r := pl.b.admit(c.length); r {
			case "":
				stopped = false
				pathBytes += len(label)
				pl.p.file = append(pl.p.file, seg)
				continue
			case "max-files":
				pl.setLimit("max-segments", "the segment budget is spent")
			case "max-bytes":
				pl.setLimit("max-bytes", "the output budget is spent")
			default:
				return fmt.Errorf("slack plan: %w: segment of %d bytes (%s)", errInvalidSize, c.length, r)
			}
		}
		pl.skip(slackLimit)
	}
	return nil
}

// volRun is the volume run being merged chunk by chunk (3F-R20).
type volRun struct {
	off, length int64
	uniform     bool
	fill        byte
	chunks      int
	open        bool
}

// planVolume plans the bytes of the partition past the end of the filesystem (3F-R8).
func (pl *slackPlanner) planVolume() error {
	size, plen := pl.p.fsSize, pl.part.Length
	switch {
	case pl.p.limit != "":
		pl.stopVolume("limit")
		pl.volSkip(slackLimit)
		pl.warn(slackLimit, "", "the volume slack was not planned: "+pl.p.limit+" was reached while planning file slack")
		return nil
	case size <= 0:
		pl.volSkip(slackSizeUnknown)
		pl.warn(slackSizeUnknown, "", "the filesystem size is unknown: no volume slack was planned")
		return nil
	case size > plen:
		pl.volSkip(slackSizeBeyondPartition)
		pl.warn(slackSizeBeyondPartition, "", fmt.Sprintf("the filesystem claims %d bytes in a %d-byte partition: no volume slack was planned", size, plen))
		return nil
	case size == plen:
		return nil
	}
	start, ok1 := filesys.AddOK(pl.part.Start, size)
	end, ok2 := filesys.AddOK(pl.part.Start, plen)
	if !ok1 || !ok2 {
		pl.volSkip(slackBeyondImage)
		pl.warn(slackBeyondImage, "", "the partition range overflows")
		return nil
	}
	if end > pl.imgSize {
		pl.volSkip(slackBeyondImage)
		pl.warn(slackBeyondImage, "", fmt.Sprintf("the partition ends %d bytes past the end of the image: the volume slack is clipped", end-pl.imgSize))
		end = pl.imgSize
	}
	if start >= end {
		return nil
	}
	buf := make([]byte, uniformChunk)
	var cur volRun
	for off := start; off < end; {
		if err := pl.ctx.Err(); err != nil {
			return err
		}
		n := int(min(int64(len(buf)), end-off))
		spent := false
		if int64(n) > pl.scanLeft { // read what the budget allows, in whole chunks, then stop
			n = int(pl.scanLeft - pl.scanLeft%SlackVolumeChunk)
			spent = true
			pl.setLimit("max-scan-bytes", "the read budget for the volume tail is spent")
			if n == 0 {
				break
			}
		}
		got, err := pl.rd.ReadAt(buf[:n], off)
		if got < n {
			if cerr := pl.ctx.Err(); cerr != nil {
				return cerr
			}
			pl.volSkip(slackReadError)
			pl.warn(slackReadError, "", fmt.Sprintf("the volume tail could not be read at offset %d: %v", off, err))
			pl.stopVolume("read-error")
			break
		}
		pl.scanLeft -= int64(n)
		for c := 0; c < n; c += SlackVolumeChunk {
			chunk := buf[c:min(c+SlackVolumeChunk, n)]
			uniform, fill := chunkUniform(chunk)
			same := cur.open && cur.uniform == uniform && (!uniform || cur.fill == fill)
			if !same {
				if !pl.flushVolume(&cur) {
					return nil
				}
				cur = volRun{off: off + int64(c), uniform: uniform, fill: fill, open: true}
			}
			cur.length += int64(len(chunk))
			cur.chunks++
		}
		off += int64(n)
		if spent {
			break
		}
	}
	pl.flushVolume(&cur)
	return nil
}

func (pl *slackPlanner) stopVolume(why string) {
	if pl.p.volStop == "" {
		pl.p.volStop = why
	}
}

// chunkUniform reports whether every byte of b equals the first, and that byte.
func chunkUniform(b []byte) (bool, byte) {
	for _, x := range b[1:] {
		if x != b[0] {
			return false, 0
		}
	}
	return true, b[0]
}

// flushVolume closes the run: it exports, counts or omits it. It reports false when a limit stopped the plan.
func (pl *slackPlanner) flushVolume(r *volRun) bool {
	if !r.open {
		return true
	}
	seg := SlackSegment{ImageOffset: r.off, Length: r.length, Uniform: r.uniform}
	if r.uniform {
		seg.Fill = r.fill
		if !pl.o.IncludeZero {
			pl.p.volUniformChunks += r.chunks
			pl.p.volUniformBytes += r.length
			if len(pl.p.volOmitted) < slackOmittedCap {
				pl.p.volOmitted = append(pl.p.volOmitted, seg)
			} else {
				pl.p.volOmittedLost++
			}
			return true
		}
	}
	if len(pl.p.vol) >= slackSegmentCap {
		pl.setLimit("max-segments", fmt.Sprintf("more than %d volume segments", slackSegmentCap))
		pl.stopVolume("limit")
		return false
	}
	switch reason := pl.b.admit(r.length); reason {
	case "":
		pl.p.vol = append(pl.p.vol, seg)
		return true
	case "max-files":
		pl.setLimit("max-segments", "the segment budget is spent")
	case "max-bytes":
		pl.setLimit("max-bytes", "the output budget is spent")
	default:
		pl.setLimit(reason, "internal error")
	}
	pl.stopVolume("limit")
	return false
}
