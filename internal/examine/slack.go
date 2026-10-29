package examine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

// AlgorithmSlack names the slack export in the provenance of its artifacts (3F-R14).
const AlgorithmSlack = "minutiae-slack/1"

// SlackSummary is the outcome of ExportSlack.
type SlackSummary struct {
	Summary                      // AnalysisID, Files (complete .bin artifacts), Bytes, Skipped, Warnings, Artifacts, FSWarnings
	Partition                    int
	FSType                       string
	Unit                         int64
	FilesExamined                int
	FileSegments, VolumeSegments int
	FileBytes, VolumeBytes       int64
	UniformSegments              int
	UniformBytes                 int64
	VolumeUniformChunks          int
	VolumeUniformBytes           int64
	SkippedBy                    map[string]int // the files by outcome; the volume part's outcomes are in the volume artifact's Params
	WalkErrors                   int
	LimitReached                 string
	Incomplete                   bool // an artifact was written incomplete (walk errors, a limit, a failed copy)
}

// slackCopyHook is a test seam: an error injected before segment i of an artifact is copied.
var slackCopyHook func(segment int) error

// slackArt is one artifact to write: its planned segments, the uniform ranges it lists as omitted (an
// incomplete one only) and whether, and why, the plan behind it is incomplete.
type slackArt struct {
	file       bool
	segs       []SlackSegment
	omitted    []SlackSegment
	incomplete bool
	errText    string
}

func (a slackArt) method() string {
	if a.file {
		return evidence.MethodSlackFile
	}
	return evidence.MethodSlackVolume
}

func (a slackArt) name() string {
	if a.file {
		return "slack"
	}
	return "volume-slack"
}

func (a slackArt) bytes() int64 {
	var n int64
	for _, s := range a.segs {
		n += s.Length // disjoint segments inside the image: the sum cannot overflow
	}
	return n
}

// ExportSlack exports the file and/or volume slack of a partition as slack artifacts (plan 3F, spec 8.3).
// A filesystem whose reader cannot state its allocation is filesys.ErrNoSlack, before any audit entry. The
// plan is built once, audited and written: each artifact's run map first, then the bytes. Walk errors and
// limits keep what was planned and flag the artifact incomplete (the run returns nil, SlackSummary.Incomplete
// is true); a failure to write to the case, or a read failure while copying, ends the run (the partial
// artifact is kept, flagged incomplete).
func (s *Session) ExportSlack(ctx context.Context, o SlackOptions) (SlackSummary, error) {
	b, err := newBudget(evidence.MaxRecoveredRuns, o.MaxBytes)
	if err != nil {
		return SlackSummary{}, err
	}
	fsys, part, err := s.FS(o.Partition)
	if err != nil {
		return SlackSummary{}, err
	}
	fsType := fsys.Info().Type
	if !filesys.ReportsSlack(fsys) {
		return SlackSummary{}, fmt.Errorf("%s: %w", fsType, filesys.ErrNoSlack)
	}
	partDir, err := evidence.SanitizeRelPath(fmt.Sprintf("p%d-%s", part.Index, fsType))
	if err != nil {
		return SlackSummary{}, fmt.Errorf("no usable directory name for partition %d (%s): %w", part.Index, fsType, err)
	}
	doFiles, doVol := o.Files || !o.Volume, o.Volume || !o.Files
	details := map[string]any{
		"parent_id": s.Parent.ID, "partition": part.Index, "fs_type": fsType, "format": AlgorithmSlack,
		"files": doFiles, "volume": doVol, "include_zero": o.IncludeZero, "max_bytes": b.maxBytes,
	}
	var ss SlackSummary
	sum, err := runAnalysis(s.Case, s.Parent.Source.DeviceID, "slack", details, func(a *analysis) error {
		defer func() {
			a.setEnd("files_examined", ss.FilesExamined)
			a.setEnd("segments", ss.FileSegments+ss.VolumeSegments)
			a.setEnd("file_segments", ss.FileSegments)
			a.setEnd("volume_segments", ss.VolumeSegments)
			a.setEnd("file_bytes", ss.FileBytes)
			a.setEnd("volume_bytes", ss.VolumeBytes)
			a.setEnd("uniform_segments", ss.UniformSegments)
			a.setEnd("uniform_bytes", ss.UniformBytes)
			a.setEnd("volume_uniform_chunks", ss.VolumeUniformChunks)
			a.setEnd("volume_uniform_bytes", ss.VolumeUniformBytes)
			a.setEnd("skipped_by", ss.SkippedBy)
			a.setEnd("walk_errors", ss.WalkErrors)
			a.setEnd("limit_reached", ss.LimitReached)
			a.setEnd("incomplete", ss.Incomplete)
		}()
		if err := a.watchFS(fsys); err != nil {
			return err
		}
		plan, err := s.planSlack(ctx, o, fsys, part, b)
		if err != nil {
			return err
		}
		ss.Partition, ss.FSType, ss.Unit = part.Index, fsType, plan.unit
		ss.FilesExamined, ss.SkippedBy, ss.WalkErrors, ss.LimitReached = plan.files, plan.fileSkipped(), plan.walkErrors, plan.limit
		ss.FileSegments = len(plan.file)
		ss.UniformSegments, ss.UniformBytes = plan.uniformSegs, plan.uniformBytes
		var arts []slackArt
		var volBytes int64
		// The volume figures are exported figures: they are set only once the volume part is reached, so a run
		// that ended before it never reports planned bytes as written.
		volSet := false
		setVol := func() {
			if volSet {
				return
			}
			volSet = true
			ss.VolumeSegments, ss.VolumeBytes = len(plan.vol), volBytes
			ss.VolumeUniformChunks, ss.VolumeUniformBytes = plan.volUniformChunks, plan.volUniformBytes
		}
		if doFiles {
			art := slackArt{file: true, segs: plan.file, incomplete: plan.fileIncomplete()}
			if art.incomplete {
				art.omitted, art.errText = plan.omitted, plan.fileErrText()
			}
			ss.FileBytes = art.bytes()
			if len(art.segs) > 0 || art.incomplete {
				arts = append(arts, art)
			}
		}
		if doVol {
			art := slackArt{segs: plan.vol, incomplete: plan.volumeIncomplete()}
			if art.incomplete {
				art.omitted, art.errText = plan.volOmitted, plan.volumeErrText()
			}
			volBytes = art.bytes()
			if len(art.segs) > 0 || art.incomplete {
				arts = append(arts, art)
			}
		}
		ss.Incomplete = (doFiles && plan.fileIncomplete()) || (doVol && plan.volumeIncomplete())
		if err := a.syncFS(); err != nil { // what planning made the filesystem notice
			return err
		}
		if err := s.auditSlackPlan(a, plan); err != nil {
			return err
		}
		if doFiles && len(plan.file) == 0 && !plan.fileIncomplete() {
			if err := a.warnWith("", "no file slack exported", map[string]any{"source": "examine"}); err != nil {
				return err
			}
		}
		if doVol && len(plan.vol) == 0 && !plan.volumeIncomplete() {
			if err := a.warnWith("", "no volume slack exported", map[string]any{"source": "examine"}); err != nil {
				return err
			}
		}
		var total int64
		for _, art := range arts {
			total += art.bytes()
		}
		if err := s.checkSpace(total); err != nil {
			return err
		}
		w := &slackWriter{
			s: s, ctx: ctx, o: o, a: a, ss: &ss, plan: plan, part: part, fsType: fsType, dir: "slack/" + partDir, total: total,
			unknown: len(fsys.Info().Warnings) > 0,
		}
		for _, art := range arts {
			if !art.file {
				setVol()
			}
			if err := w.write(art); err != nil {
				return err
			}
		}
		setVol()
		return nil
	})
	ss.Summary = sum
	return ss, err
}

// fileErrText names why the file part of the plan is incomplete.
func (p *slackPlan) fileErrText() string {
	var parts []string
	if p.walkErrors > 0 {
		t := "1 directory not read to the end"
		if p.walkErrors > 1 {
			t = fmt.Sprintf("%d directories not read to the end", p.walkErrors)
		}
		parts = append(parts, t+" (first: "+p.firstWalkError+")")
	}
	if p.limit != "" {
		parts = append(parts, "limit reached: "+p.limit)
	}
	if p.unchecked > 0 {
		parts = append(parts, fmt.Sprintf("%d live file(s) could not be opened for the shared-unit check: their allocation units are unknown", p.unchecked))
	}
	return strings.Join(parts, "; ")
}

// volumeErrText names why the volume part of the plan is incomplete.
func (p *slackPlan) volumeErrText() string {
	switch {
	case p.limit != "":
		return "limit reached: " + p.limit
	case p.volStop == "read-error":
		return "the volume tail could not be read to the end"
	}
	return "the volume part was not planned to the end: " + p.volStop
}

// auditSlackPlan writes the filesystem note and every plan warning as analysis.warning entries (source
// examine), in plan order. The note is not counted as a skip, the warnings are.
func (s *Session) auditSlackPlan(a *analysis, plan *slackPlan) error {
	if plan.fsNote != "" {
		d := map[string]any{"analysis_id": a.sum.AnalysisID, "source": "examine", "path": "", "reason": plan.fsNote, "warning": plan.fsNote}
		if err := a.appendWarning(d); err != nil {
			return err
		}
	}
	for _, w := range plan.warnings {
		if err := a.warnWith(w.Path, w.Reason, map[string]any{"source": "examine", "warning": w.Reason}); err != nil {
			return err
		}
	}
	return nil
}

type slackWriter struct {
	s       *Session
	ctx     context.Context
	o       SlackOptions
	a       *analysis
	ss      *SlackSummary
	plan    *slackPlan
	part    volume.Partition
	fsType  string
	dir     string
	total   int64
	copied  int64
	unknown bool // file slack is counted Unknown when the filesystem has warnings (3A A10)
}

// slackParams is the Params of a slack artifact (3F-R14): the counters of the plan, as strings. case verify
// recomputes them from a re-plan with this same function, so writer and check cannot drift.
func slackParams(p *slackPlan, file, includeZero bool) map[string]string {
	par := map[string]string{"include_zero": strconv.FormatBool(includeZero)}
	if file {
		par["unit"] = strconv.FormatInt(p.unit, 10)
		par["files"] = strconv.Itoa(p.files)
		par["segments"] = strconv.Itoa(len(p.file))
		par["uniform_segments"] = strconv.Itoa(p.uniformSegs)
		par["uniform_bytes"] = strconv.FormatInt(p.uniformBytes, 10)
		par["walk_errors"] = strconv.Itoa(p.walkErrors)
		for r, c := range p.fileSkipped() {
			par["skipped_"+strings.ReplaceAll(r, "-", "_")] = strconv.Itoa(c)
		}
		if p.omittedLost > 0 {
			par["omitted_unlisted"] = strconv.Itoa(p.omittedLost)
		}
		return par
	}
	par["chunk"] = strconv.Itoa(SlackVolumeChunk)
	par["fs_size"] = strconv.FormatInt(p.fsSize, 10)
	par["partition_length"] = strconv.FormatInt(p.part.Length, 10)
	par["segments"] = strconv.Itoa(len(p.vol))
	par["uniform_chunks"] = strconv.Itoa(p.volUniformChunks)
	par["uniform_bytes"] = strconv.FormatInt(p.volUniformBytes, 10)
	for r, c := range p.volSkipped {
		par["skipped_"+strings.ReplaceAll(r, "-", "_")] = strconv.Itoa(c)
	}
	if p.volOmittedLost > 0 {
		par["omitted_unlisted"] = strconv.Itoa(p.volOmittedLost)
	}
	return par
}

// recovery builds the provenance of an artifact (3F-R14).
func (w *slackWriter) recovery(art slackArt) *evidence.Recovery {
	p := w.plan
	conf := 10
	rv := &evidence.Recovery{
		Class: evidence.ClassSlack, Method: art.method(), Confidence: &conf, Content: "ok", Algorithm: AlgorithmSlack,
		Params: slackParams(p, art.file, w.o.IncludeZero),
	}
	n := art.bytes()
	if art.file {
		rv.Basis = []string{"allocated-units-of-live-files"}
		rv.Alloc = evidence.AllocSummary{Allocated: n}
		if w.unknown {
			rv.Alloc = evidence.AllocSummary{Unknown: n}
		}
		if p.unchecked > 0 { // the overlap check was incomplete: the segments keep that assumption
			rv.Assumptions = []string{slackUncheckedTag}
		}
		return rv
	}
	rv.Basis = []string{fmt.Sprintf("filesystem-size=%d", p.fsSize), fmt.Sprintf("partition-length=%d", p.part.Length)}
	rv.Alloc = evidence.AllocSummary{Unknown: n}
	return rv
}

// writeSlackMap writes the run map: the exported lines in order, then (an incomplete artifact only) the
// omitted uniform ranges at the final offset, which they do not advance.
func writeSlackMap(w io.Writer, art slackArt, segs, omitted []SlackSegment) error {
	bw := bufio.NewWriter(w)
	var off int64
	line := func(g SlackSegment, exported bool) error {
		l := evidence.SlackMapLine{Offset: off, Length: g.Length, ImageOffset: g.ImageOffset, Uniform: g.Uniform, Exported: exported}
		if art.file {
			l.FSPath, l.PathEscaped, l.FSID, l.FileSize = g.FSPath, g.PathEscaped, g.FSID, g.FileSize
		}
		if exported {
			off += g.Length
		}
		return evidence.WriteSlackMapLine(bw, art.method(), l)
	}
	for _, g := range segs {
		if err := line(g, true); err != nil {
			return err
		}
	}
	for _, g := range omitted {
		if err := line(g, false); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// prefixSegments returns the segments that cover the first n bytes of an artifact, the last one cut.
func prefixSegments(segs []SlackSegment, n int64) []SlackSegment {
	var out []SlackSegment
	for _, g := range segs {
		if n <= 0 {
			break
		}
		if g.Length > n {
			g.Length = n
		}
		out = append(out, g)
		n -= g.Length
	}
	return out
}

// write stores one artifact: its run map, then its bytes.
func (w *slackWriter) write(art slackArt) error {
	s, a := w.s, w.a
	base := w.dir + "/" + art.name()
	d := s.baseDerivation(w.part, w.fsType)
	rv := w.recovery(art)
	d.Recovery = rv

	runs := make([]evidence.Run, len(art.segs))
	for i, g := range art.segs {
		runs[i] = evidence.Run{Offset: g.ImageOffset, Length: g.Length}
	}
	src := evidence.Source{Kind: evidence.KindSlack, DeviceID: s.Parent.Source.DeviceID, Derived: d}
	// Nothing that case verify would reject is ever written: a failure here is a defect of this build.
	probe := evidence.ManifestRecord{Path: base + ".bin", Size: art.bytes(), Incomplete: art.incomplete, Error: art.errText, Source: src}
	if probs := append(rv.Check(evidence.KindSlack), evidence.CheckRecoveredRuns(probe, runs)...); len(probs) > 0 {
		return fmt.Errorf("internal error: the %s artifact would not pass verification: %s", art.name(), strings.Join(probs, "; "))
	}

	sd := *d
	sd.Recovery = nil // a runs sidecar describes runs, the recovery stays on the content artifact
	side, fillErr, err := s.capture(a, base+".runs.jsonl", evidence.Source{Kind: "runs", DeviceID: src.DeviceID, Derived: &sd}, func(out io.Writer) error {
		return writeSlackMap(out, art, art.segs, art.omitted)
	})
	if err != nil {
		return err
	}
	if fillErr != nil { // generated in memory: only the case can fail here
		return &caseWriteError{fillErr}
	}
	d.RunsArtifact = side.ID

	rec, fillErr, err := s.capture(a, base+".bin", src, func(out io.Writer) error {
		return w.copy(out, base, art, d, rv)
	})
	if err != nil {
		return err
	}
	var cut *cutError
	switch {
	case fillErr == nil:
		a.sum.Files++
		a.sum.Bytes += rec.Size
		return nil
	case errors.As(fillErr, &cut):
		return nil // a planned incompleteness: described by the artifact itself
	}
	w.ss.Incomplete = true
	return fillErr // the partial artifact is kept, flagged incomplete
}

// copy streams the segments from the image into out. A failure after n bytes narrows the recorded
// provenance to the bytes written (a prefix run map, the full one kept as the declared list); a planned
// incompleteness ends with a cutError after all segments.
func (w *slackWriter) copy(out io.Writer, base string, art slackArt, d *evidence.Derivation, rv *evidence.Recovery) error {
	tw := &trackWriter{w: out}
	var n int64
	var ut *uniWriter // the bytes written of the segment being copied
	narrow := func() error {
		if n >= art.bytes() {
			return nil
		}
		rv.Alloc = evidence.AllocSummary{Allocated: n}
		if !art.file || w.unknown {
			rv.Alloc = evidence.AllocSummary{Unknown: n}
		}
		rv.Assumptions = append(rv.Assumptions, fmt.Sprintf("read-failed-after=%d", n))
		declared := d.RunsArtifact
		id, err := w.writePrefixMap(base, art, d, n, ut)
		if err != nil {
			return err
		}
		d.Runs, d.RunsArtifact = nil, id
		if declared != "" && declared != id {
			rv.DeclaredRunsArtifact = declared
		}
		return nil
	}
	buf := make([]byte, copyBufSize)
	for i, g := range art.segs {
		ut = &uniWriter{w: tw, uniform: true}
		if slackCopyHook != nil {
			if err := slackCopyHook(i); err != nil {
				if nerr := narrow(); nerr != nil {
					return nerr
				}
				return err
			}
		}
		m, err := io.CopyBuffer(ut, &ctxReader{ctx: w.ctx, r: slackCutReader(i, io.NewSectionReader(w.s.Image, g.ImageOffset, g.Length)), onRead: func(k int) {
			w.copied += int64(k)
			if w.o.Progress != nil {
				w.o.Progress(w.copied, w.total)
			}
		}}, buf)
		n += m
		switch {
		case tw.err != nil:
			if nerr := narrow(); nerr != nil {
				return nerr
			}
			return &caseWriteError{tw.err}
		case err != nil:
			if nerr := narrow(); nerr != nil {
				return nerr
			}
			return err
		case m != g.Length:
			if nerr := narrow(); nerr != nil {
				return nerr
			}
			return fmt.Errorf("image ended after %d of %d bytes of the segment at %d: %w", m, g.Length, g.ImageOffset, io.ErrUnexpectedEOF)
		}
	}
	if art.incomplete {
		return &cutError{art.errText}
	}
	return nil
}

// writePrefixMap stores the run map of the first n bytes of an artifact under the first free of the names
// <base>.bin.incomplete.runs.jsonl, <base>.bin.incomplete~2.runs.jsonl ... and returns its id. A name
// already taken is no case failure; only a failure of the case is returned. No free name found, it returns
// an empty id: the artifact then carries no runs, which is incomplete provenance but never wrong provenance.
func (w *slackWriter) writePrefixMap(base string, art slackArt, d *evidence.Derivation, n int64, cut *uniWriter) (string, error) {
	pre := prefixSegments(art.segs, n)
	// The cut last line describes the bytes actually written: their uniformity, not the declared segment's.
	if k := len(pre); k > 0 && cut != nil && cut.n > 0 && pre[k-1].Length < art.segs[k-1].Length {
		pre[k-1].Uniform, pre[k-1].Fill = cut.uniform, cut.first
	}
	for attempt := 0; attempt < maxNameAttempts; attempt++ {
		name := base + ".bin.incomplete.runs.jsonl"
		if attempt > 0 {
			name = fmt.Sprintf("%s.bin.incomplete~%d.runs.jsonl", base, attempt+1)
		}
		sd := *d
		sd.Runs, sd.RunsArtifact, sd.Recovery, sd.AcquisitionGaps = nil, "", nil, nil
		rec, fillErr, err := w.s.capture(w.a, name, evidence.Source{Kind: "runs", DeviceID: w.s.Parent.Source.DeviceID, Derived: &sd}, func(out io.Writer) error {
			return writeSlackMap(out, art, pre, nil)
		})
		var nameErr *localNameError
		switch {
		case err == nil && fillErr != nil:
			return "", &caseWriteError{fillErr}
		case err == nil:
			return rec.ID, nil
		case errors.Is(err, evidence.ErrArtifactExists), errors.As(err, &nameErr):
			continue
		}
		return "", err
	}
	return "", nil
}

// uniWriter passes writes on and remembers whether every byte written so far is equal, and the first byte.
type uniWriter struct {
	w       io.Writer
	n       int64
	first   byte
	uniform bool
}

func (u *uniWriter) Write(p []byte) (int, error) {
	n, err := u.w.Write(p)
	for _, b := range p[:n] {
		if u.n == 0 {
			u.first = b
		} else if b != u.first {
			u.uniform = false
		}
		u.n++
	}
	return n, err
}

// slackCutAfter is a test seam: the number of bytes of segment i delivered before a read failure (negative
// or nil: none), to cut a segment in the middle.
var slackCutAfter func(segment int) int64

// slackCutReader applies slackCutAfter to the reader of segment i.
func slackCutReader(i int, r io.Reader) io.Reader {
	if slackCutAfter == nil {
		return r
	}
	k := slackCutAfter(i)
	if k < 0 {
		return r
	}
	return io.MultiReader(io.LimitReader(r, k), errReader{errors.New("injected read failure inside a segment")})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
