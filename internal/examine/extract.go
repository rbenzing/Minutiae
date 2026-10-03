package examine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

// maxNameAttempts bounds the local-name retries for one extracted file.
const maxNameAttempts = 16

// ExtractOptions select what Extract copies out of a filesystem.
type ExtractOptions struct {
	Partition int      // -1 = auto
	Paths     []string // fs paths ("/a/b") or "id:<fs id>"
	Recursive bool
	Progress  func(done, total int64) // total -1 when unknown
}

// Extract copies files (and symlink targets) out of the image's filesystem
// into the case as derived artifacts. Each artifact records its provenance
// (evidence.Derivation) and the run is bracketed by analysis.start and
// analysis.end/analysis.error audit entries. Deleted entries and entries that
// cannot be read are skipped with an analysis.warning; a failure to write
// the case, or a cancelled ctx, aborts the run (a partially written artifact
// is kept and flagged incomplete).
func (s *Session) Extract(ctx context.Context, o ExtractOptions) (Summary, error) {
	if len(o.Paths) == 0 {
		return Summary{}, errors.New("nothing to extract: no paths given")
	}
	fsys, part, err := s.FS(o.Partition)
	if err != nil {
		return Summary{}, err
	}
	type target struct {
		e filesys.Entry
		p string
	}
	targets := make([]target, 0, len(o.Paths))
	for _, ref := range o.Paths {
		e, p, err := s.Lookup(fsys, ref)
		if err != nil {
			return Summary{}, err
		}
		if e.Type == filesys.TypeDir && !e.Deleted && !o.Recursive {
			return Summary{}, fmt.Errorf("%s is a directory (use -r)", p)
		}
		targets = append(targets, target{e, p})
	}

	fsType := fsys.Info().Type
	x := &extractor{
		s: s, ctx: ctx, o: o, fsys: fsys, part: part, fsType: fsType,
		lp:   evidence.NewLocalPaths(fmt.Sprintf("p%d-%s", part.Index, fsType)),
		done: map[doneKey]bool{},
	}
	details := map[string]any{
		"parent_id": s.Parent.ID, "partition": part.Index, "fs_type": fsType,
		"paths": append([]string(nil), o.Paths...), "recursive": o.Recursive,
	}
	return runAnalysis(s.Case, s.Parent.Source.DeviceID, "extract", details, func(a *analysis) error {
		x.a = a
		if err := a.watchFS(fsys); err != nil {
			return err
		}
		for _, t := range targets {
			if err := x.target(t.p, t.e); err != nil {
				return err
			}
		}
		return nil
	})
}

// doneKey identifies an extracted entry: the same path can name different
// entries (a deleted and a live one, or hard links), so the entry id counts.
type doneKey struct{ path, id string }

type extractor struct {
	s      *Session
	ctx    context.Context
	o      ExtractOptions
	fsys   filesys.FileSystem
	part   volume.Partition
	fsType string
	lp     *evidence.LocalPaths
	a      *analysis
	done   map[doneKey]bool // entries already extracted in this run
	copied int64            // bytes streamed so far, for Progress
}

const sparseTooBig = "sparse size exceeds partition length; not extracted"

const deletedReason = "deleted; recovery is roadmap sub-project 3"

// target handles one requested entry: a file or symlink, or a directory to walk.
func (x *extractor) target(p string, e filesys.Entry) error {
	if err := x.ctx.Err(); err != nil {
		return err
	}
	switch {
	case e.Deleted:
		return x.a.warn(p, deletedReason)
	case e.Type == filesys.TypeDir:
		return filesys.Walk(x.fsys, e, p, func(wp string, we filesys.Entry, werr error) error {
			if werr != nil {
				return x.a.warn(wp, "directory not walked: "+werr.Error())
			}
			if we.Type == filesys.TypeDir && !we.Deleted {
				return nil // its children follow
			}
			return x.target(wp, we)
		})
	case e.Type == filesys.TypeFile || e.Type == filesys.TypeSymlink:
		return x.file(p, e)
	default:
		return x.a.warn(p, "not a regular file, directory or symlink ("+e.Type.String()+")")
	}
}

// skippable reports whether err is a problem of one entry's content (so the
// run can go on) rather than of the case or the run itself.
func skippable(err error) bool {
	for _, target := range []error{
		filesys.ErrCorrupt, filesys.ErrUnsupported, filesys.ErrEncrypted, filesys.ErrDeleted, filesys.ErrNotFound,
		io.ErrUnexpectedEOF, io.EOF,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// caseWriteError marks a failure to write into the case (as opposed to a
// failure to read the source), which always aborts the run.
type caseWriteError struct{ err error }

func (e *caseWriteError) Error() string { return "write to case: " + e.err.Error() }
func (e *caseWriteError) Unwrap() error { return e.err }

// trackWriter remembers whether the destination failed.
type trackWriter struct {
	w   io.Writer
	err error
}

func (t *trackWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil && t.err == nil {
		t.err = err
	}
	return n, err
}

// fatal reports whether err ends the whole run.
func fatal(ctx context.Context, err error) bool {
	var cw *caseWriteError
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &cw)
}

func (x *extractor) file(p string, e filesys.Entry) error {
	if err := x.fileWork(p, e); err != nil {
		return err
	}
	return x.a.syncFS() // what reading this file made the filesystem notice
}

func (x *extractor) fileWork(p string, e filesys.Entry) error {
	key := doneKey{p, e.ID}
	if x.done[key] {
		return x.a.warn(p, "already extracted in this analysis (overlapping paths)")
	}
	x.done[key] = true
	if e.Name == "" || e.Name == "." || e.Name == ".." || strings.Contains(e.Name, "/") {
		return x.a.warn(p, fmt.Sprintf("unsafe entry name %q", e.Name))
	}
	f, err := x.fsys.Open(e)
	if err != nil {
		if skippable(err) {
			return x.a.warn(p, "not extracted: "+err.Error())
		}
		return err
	}
	size := f.Size()
	if holeBytes(f.Runs(), size) > x.part.Length {
		// A sparse file reads as zeros without any storage, so a hostile
		// filesystem could make one cheap entry expand to an arbitrary size.
		return x.a.warn(p, sparseTooBig)
	}

	d := x.s.baseDerivation(x.part, x.fsType)
	d.FSPath, d.FSID = p, e.ID
	d.Mode, d.UID, d.GID = e.Mode, e.UID, e.GID
	d.Times = timesMap(e.Times)
	d.Encrypted = e.Encrypted
	runs, runsErr := imageRuns(f.Runs(), size, x.part, x.s.Image.Size())
	if runsErr != nil {
		// Never record wrong provenance: no runs at all, content still extracted.
		if err := x.a.warn(p, "no runs recorded (content is still extracted): "+runsErr.Error()); err != nil {
			return err
		}
		runs = nil
	}
	useSidecar := len(runs) > evidence.MaxInlineRuns
	if !useSidecar {
		d.Runs = runs
	}
	deviceID := x.s.Parent.Source.DeviceID
	src := evidence.Source{Kind: "extract", DeviceID: deviceID, RemotePath: p, Derived: d}

	dir, err := x.lp.Dir(path.Dir(p))
	if err != nil {
		return x.a.warn(p, "no usable local directory name: "+err.Error())
	}
	var (
		rec     evidence.ManifestRecord
		readErr error
	)
	for attempt := 0; ; attempt++ {
		rel, err := x.lp.File(dir, e.Name)
		if err != nil {
			return x.a.warn(p, "no usable local name: "+err.Error())
		}
		if useSidecar {
			err = x.s.writeRunsSidecar(x.a, rel+".runs.jsonl", runs, p, d)
		}
		if err == nil {
			rec, readErr, err = x.s.capture(x.a, rel, src, func(w io.Writer) error {
				return x.copyFile(w, f, size)
			})
		}
		if !errors.Is(err, evidence.ErrArtifactExists) {
			if err != nil {
				return err // the case itself failed: never downgraded to a warning
			}
			break
		}
		// The examiner's filesystem aliases the name in a way LocalPaths does
		// not model (a runs sidecar of another file, NTFS 8.3 short names,
		// APFS NFC/NFD). The exclusive create failed before any byte was read,
		// the colliding name stays reserved and the next candidate is tried.
		if attempt+1 >= maxNameAttempts {
			return x.a.warn(p, fmt.Sprintf("no free local name after %d attempts: %v", attempt+1, err))
		}
	}
	switch {
	case readErr == nil:
		x.a.sum.Files++
		x.a.sum.Bytes += rec.Size
		return nil
	case fatal(x.ctx, readErr) || !skippable(readErr):
		return readErr
	}
	return x.a.warn(p, fmt.Sprintf("read failed after %d of %d bytes (partial artifact kept, flagged incomplete): %v", rec.Size, size, readErr))
}

// capture writes one artifact: fill streams its content. fillErr is the error
// of fill alone (the artifact is then kept, flagged incomplete), so the caller
// classifies it without any case failure mixed in. err is a failure of the
// case itself (creating the artifact, closing or aborting it, recording it in
// the manifest, database or audit log) and is a *caseWriteError, except
// evidence.ErrArtifactExists, which is returned as is so the caller can try
// another name.
func (s *Session) capture(a *analysis, rel string, src evidence.Source, fill func(io.Writer) error) (rec evidence.ManifestRecord, fillErr, err error) {
	w, err := s.Case.NewArtifact(src.DeviceID, a.sum.AnalysisID, rel, src)
	if errors.Is(err, evidence.ErrArtifactExists) {
		return rec, nil, err
	}
	if err != nil {
		return rec, nil, &caseWriteError{err}
	}
	if fillErr = fill(w); fillErr == nil {
		rec, err = w.Close()
	} else {
		rec, err = w.Abort(fillErr)
	}
	a.add(rec)
	if err != nil {
		return rec, fillErr, &caseWriteError{err}
	}
	return rec, fillErr, nil
}

// copyFile streams the content of f into w, checking ctx on every read.
func (x *extractor) copyFile(w io.Writer, f filesys.File, size int64) error {
	tw := &trackWriter{w: w}
	n, err := io.Copy(tw, &ctxReader{ctx: x.ctx, r: io.NewSectionReader(f, 0, size), onRead: func(n int) {
		x.copied += int64(n)
		if x.o.Progress != nil {
			x.o.Progress(x.copied, -1)
		}
	}})
	switch {
	case tw.err != nil:
		return &caseWriteError{tw.err}
	case err != nil:
		return err
	case n != size:
		return fmt.Errorf("content ended after %d of %d bytes: %w", n, size, io.ErrUnexpectedEOF)
	}
	return nil
}

// baseDerivation returns the parent fields shared by every derived artifact
// of one partition (Partition 0 and offset 0 mean the whole image).
func (s *Session) baseDerivation(part volume.Partition, fsType string) *evidence.Derivation {
	d := &evidence.Derivation{
		ParentID: s.Parent.ID, ParentSHA256: s.Parent.SHA256,
		Partition: part.Index, PartitionOffset: part.Start, FSType: fsType,
	}
	d.ParentSegments = s.ParentSegments()
	for _, seg := range s.Segments {
		d.ParentIncomplete = d.ParentIncomplete || seg.Incomplete
	}
	return d
}

// holeBytes returns how many bytes of a file of size bytes are not backed by
// storage: the sum of its hole runs (Offset -1), saturating at MaxInt64, or,
// for a file without runs (content inline in metadata, or entirely sparse),
// its whole size. Inline content is far smaller than any partition, so only a
// genuinely sparse file can exceed a partition's length.
func holeBytes(runs []filesys.Run, size int64) int64 {
	if len(runs) == 0 {
		return max(size, 0)
	}
	var total int64
	for _, r := range runs {
		if r.Offset >= 0 || r.Length <= 0 {
			continue
		}
		var ok bool
		if total, ok = filesys.AddOK(total, r.Length); !ok {
			return math.MaxInt64
		}
	}
	return total
}

// imageRuns validates the filesystem-relative runs of a file of size bytes and
// converts them to image-relative ones (holes stay -1). Every run must lie inside both the partition and the image. No runs (inline
// content) yields none.
func imageRuns(raw []filesys.Run, size int64, part volume.Partition, imageSize int64) ([]evidence.Run, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if err := filesys.CheckRuns(raw, size, part.Length); err != nil {
		return nil, err
	}
	out := make([]evidence.Run, 0, len(raw))
	for _, r := range raw {
		switch {
		case r.Length == 0:
		case r.Offset < 0:
			out = append(out, evidence.Run{Offset: -1, Length: r.Length})
		default:
			off, ok := filesys.AddOK(r.Offset, part.Start)
			if !ok {
				return nil, fmt.Errorf("run offset %d + partition start %d overflows", r.Offset, part.Start)
			}
			if end, ok := filesys.AddOK(off, r.Length); !ok || end > imageSize {
				return nil, fmt.Errorf("run %d+%d lies beyond the %d-byte image", off, r.Length, imageSize)
			}
			out = append(out, evidence.Run{Offset: off, Length: r.Length})
		}
	}
	return out, nil
}

// writeRunsSidecar stores runs (one JSON evidence.Run per line) as a runs
// artifact belonging to the derived file at fsPath and points d at it. The
// content is generated in memory, so any failure writing it is a case failure.
func (s *Session) writeRunsSidecar(a *analysis, rel string, runs []evidence.Run, fsPath string, d *evidence.Derivation) error {
	sd := *d
	sd.Runs, sd.RunsArtifact = nil, ""
	src := evidence.Source{Kind: "runs", DeviceID: s.Parent.Source.DeviceID, RemotePath: fsPath, Derived: &sd}
	rec, fillErr, err := s.capture(a, rel, src, func(w io.Writer) error {
		return writeJSONLines(w, len(runs), func(i int) any { return runs[i] })
	})
	if err == nil && fillErr != nil {
		err = &caseWriteError{fillErr}
	}
	if err == nil {
		d.RunsArtifact = rec.ID
	}
	return err
}

// writeJSONLines writes n JSON values, one per line.
func writeJSONLines(w io.Writer, n int, at func(i int) any) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw) // Encode appends the newline
	for i := range n {
		if err := enc.Encode(at(i)); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// timesMap renders the non-zero timestamps: RFC 3339 (nano, UTC) when the
// zone is known, the same layout without a zone otherwise.
func timesMap(t filesys.Times) map[string]string {
	m := map[string]string{}
	for k, ts := range map[string]filesys.Timestamp{
		"modified": t.Modified, "accessed": t.Accessed, "changed": t.Changed, "created": t.Created, "deleted": t.Deleted,
	} {
		switch {
		case ts.T.IsZero():
		case ts.ZoneKnown:
			m[k] = ts.T.UTC().Format(time.RFC3339Nano)
		default:
			m[k] = ts.T.Format("2006-01-02T15:04:05.999999999")
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
