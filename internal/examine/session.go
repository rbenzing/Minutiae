// Package examine opens image artifacts that live in a case (disk or partition
// images, possibly split across several segment artifacts) and exposes their
// partition table and filesystems for read-only examination. It also imports
// external image files into a case as hashed, audited artifacts.
//
// Source artifacts are only ever opened read-only (evidence.Case.OpenArtifact);
// nothing in this package writes to an existing artifact.
package examine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/volume"
)

// Options tune a Session.
type Options struct {
	// Drivers overrides detect.Drivers (tests inject fstest). nil = detect.Drivers.
	Drivers []detect.Driver
}

// Session is one opened image artifact.
type Session struct {
	Case     *evidence.Case
	Parent   evidence.ManifestRecord   // segment 1
	Segments []evidence.ManifestRecord // in segment order (len 1 for a single artifact)
	Image    image.Image
	Table    *volume.Table // nil until ReadPartitions succeeds (Open does it)

	opts     Options
	tableErr error // the last ReadPartitions failure

	// newArtifact creates artifacts when set (tests inject faults); nil means Case.NewArtifact.
	newArtifact func(deviceID, acqID, rel string, src evidence.Source) (*evidence.ArtifactWriter, error)

	mu        sync.Mutex
	fsCache   map[int]*fsEntry
	closeOnce sync.Once
	closeErr  error
}

type fsEntry struct {
	fs    filesys.FileSystem
	err   error
	name  string   // driver that recognized the partition, "" when none did
	notes []string // probe panics of drivers tried before the final one
}

// Open resolves ref (artifact id or case-relative path), collects all segments
// of its import (same device id and acquisition directory, Source.Segment
// 1..N, contiguous; a gap or a missing file is evidence.ErrIntegrity), opens
// them read-only, opens the image and reads its partition table
// (OpenContainer followed by ReadPartitions).
func Open(c *evidence.Case, ref string, opts Options) (*Session, error) {
	s, err := OpenContainer(c, ref, opts)
	if err != nil {
		return nil, err
	}
	if err := s.ReadPartitions(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// OpenContainer is Open without the partition table: it opens the image only
// (Session.Table is nil until ReadPartitions succeeds). Container-level
// operations such as VerifyContainer need nothing more, so a damaged partition
// table cannot keep a damaged image from being verified.
func OpenContainer(c *evidence.Case, ref string, opts Options) (*Session, error) {
	ref0, err := c.FindArtifact(ref)
	if err != nil {
		return nil, err
	}
	segs := []evidence.ManifestRecord{ref0}
	if ref0.Source.Kind == "import" && ref0.Source.Segment > 0 {
		recs, err := c.Manifest()
		if err != nil {
			return nil, fmt.Errorf("manifest unreadable: %w", err)
		}
		if segs, err = collectSegments(recs, ref0); err != nil {
			return nil, err
		}
	}

	files := make([]*os.File, 0, len(segs))
	for _, seg := range segs {
		f, rec, err := c.OpenArtifact(seg.ID)
		if err != nil {
			closeFiles(files)
			return nil, err
		}
		if rec.ID != seg.ID {
			_ = f.Close()
			closeFiles(files)
			return nil, fmt.Errorf("%w: artifact %s resolved to a different record", evidence.ErrIntegrity, seg.ID)
		}
		files = append(files, f)
	}
	img, err := openImage(files)
	if err != nil {
		return nil, fmt.Errorf("open image %s: %w", segs[0].Path, err)
	}
	return &Session{
		Case: c, Parent: segs[0], Segments: segs, Image: img,
		opts: opts, fsCache: map[int]*fsEntry{},
	}, nil
}

// ReadPartitions reads the image's partition table into s.Table. On failure
// Table stays nil, the error is kept for Info (ImageInfo.PartitionError) and
// the session stays usable for container-level operations.
func (s *Session) ReadPartitions() error {
	// Raw containers report the default 512 without knowing it, and an E01 that
	// declares 512 only echoes what raw-input acquisition writes (also for 4096-byte
	// sector disks): let the partition reader probe 512 and 4096 for both. A
	// declared 1024, 2048 or 4096 is trusted.
	tbl, err := readTable(s.Image, probeSectorSize(s.Image))
	if err != nil {
		s.tableErr = fmt.Errorf("read partition table of %s: %w", s.Parent.Path, err)
		return s.tableErr
	}
	s.Table, s.tableErr = tbl, nil
	return nil
}

// probeSectorSize returns the sector size volume.Read should be told: 0 (probe
// 512, then 4096) when the container does not know it, else the declared size.
func probeSectorSize(img image.Image) int {
	switch img.Format() {
	case "raw", "split-raw":
		return 0
	case "ewf":
		if img.SectorSize() == 512 {
			return 0
		}
	}
	return img.SectorSize()
}

// openImage is image.OpenFiles with parser panics (a container opener reads
// hostile bytes) converted to a *filesys.CorruptError. The files are closed on
// every failure.
func openImage(files []*os.File) (img image.Image, err error) {
	defer func() {
		if p := recover(); p != nil {
			closeFiles(files)
			img, err = nil, panicError("image container", "open", p)
		}
	}()
	img, err = image.OpenFiles(files) // takes ownership of files, closing them on error
	if pe := new(image.PanicError); errors.As(err, &pe) {
		return nil, panicError("image container", "open", pe.Value) // the opener panic, already recovered and closed by image
	}
	return img, err
}

// readTable is volume.Read with panics (including ones raised by the image's
// ReadAt) converted to a *filesys.CorruptError.
func readTable(img image.Image, sectorSize int) (t *volume.Table, err error) {
	defer func() {
		if p := recover(); p != nil {
			t, err = nil, panicError("partition table", "read", p)
		}
	}()
	return volume.Read(img, img.Size(), sectorSize)
}

// maxSegments bounds the segment count accepted from a manifest record.
const maxSegments = 1 << 16

// acquisitionDir is artifacts/<device>/<acquisition> of a manifest path.
func acquisitionDir(p string) string {
	parts := strings.SplitN(p, "/", 4)
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return strings.Join(parts, "/")
}

// collectSegments returns the records of the import that ref belongs to, in
// segment order. Records belong to one import when they are Kind "import" with
// a segment number and share the device id and the acquisition directory. All
// of them must agree on the total segment count N, and exactly the segments
// 1..N must be present, otherwise the image is partial and the error is an
// evidence.ErrIntegrity naming what is missing or inconsistent (the examiner
// re-imports).
func collectSegments(recs []evidence.ManifestRecord, ref evidence.ManifestRecord) ([]evidence.ManifestRecord, error) {
	dir := acquisitionDir(ref.Path)
	var group []evidence.ManifestRecord
	for _, r := range recs {
		if r.Source.Kind == "import" && r.Source.Segment > 0 && r.Source.DeviceID == ref.Source.DeviceID && acquisitionDir(r.Path) == dir {
			group = append(group, r)
		}
	}
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%w: image import %s: %s; re-import the image", evidence.ErrIntegrity, dir, fmt.Sprintf(format, a...))
	}
	n := ref.Source.Segments
	if n < 1 || n > maxSegments {
		return nil, fail("segment %d of %s records an invalid total segment count %d", ref.Source.Segment, ref.Path, n)
	}
	var odd []string
	have := make(map[int][]evidence.ManifestRecord)
	for _, r := range group {
		if r.Source.Segments != n {
			odd = append(odd, fmt.Sprintf("%s (segment %d says %d segments)", r.Path, r.Source.Segment, r.Source.Segments))
			continue
		}
		have[r.Source.Segment] = append(have[r.Source.Segment], r)
	}
	if len(odd) > 0 {
		return nil, fail("inconsistent segment counts, expected %d: %s", n, strings.Join(odd, ", "))
	}
	out := make([]evidence.ManifestRecord, 0, n)
	var missing []string
	for i := 1; i <= n; i++ {
		switch len(have[i]) {
		case 0:
			if len(missing) < 16 {
				missing = append(missing, strconv.Itoa(i))
			}
		case 1:
			out = append(out, have[i][0])
		default:
			return nil, fail("segment %d is recorded %d times", i, len(have[i]))
		}
	}
	for seg := range have {
		if seg > n {
			return nil, fail("segment %d is outside 1..%d", seg, n)
		}
	}
	if len(missing) > 0 {
		return nil, fail("segment(s) %s of %d missing from the manifest", strings.Join(missing, ", "), n)
	}
	return out, nil
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// ParentSegments lists every segment of a multi-segment parent in order
// (including segment 1) for derivation provenance; nil for a single artifact.
func (s *Session) ParentSegments() []evidence.SegmentRef {
	if len(s.Segments) < 2 {
		return nil
	}
	refs := make([]evidence.SegmentRef, len(s.Segments))
	for i, seg := range s.Segments {
		refs[i] = evidence.SegmentRef{ID: seg.ID, SHA256: seg.SHA256}
	}
	return refs
}

func (s *Session) drivers() []detect.Driver {
	if s.opts.Drivers != nil {
		return s.opts.Drivers
	}
	return detect.Drivers
}

// Close releases the image (and with it every segment file handle).
func (s *Session) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.Image.Close() })
	return s.closeErr
}

func (s *Session) section(p volume.Partition) *io.SectionReader {
	return io.NewSectionReader(s.Image, p.Start, p.Length)
}

// partitionByIndex returns the partition with the given volume index.
func (s *Session) partitionByIndex(index int) (volume.Partition, bool) {
	for _, p := range s.partitions() {
		if p.Index == index {
			return p, true
		}
	}
	return volume.Partition{}, false
}

// Partition returns the partition by index; index -1 means "the only partition
// holding a recognized filesystem" (an error listing candidates when there are
// none or several).
func (s *Session) Partition(index int) (volume.Partition, error) {
	if err := s.needTable(); err != nil {
		return volume.Partition{}, err
	}
	if index != -1 {
		if p, ok := s.partitionByIndex(index); ok {
			return p, nil
		}
		return volume.Partition{}, fmt.Errorf("no partition %d (have %s)", index, indexList(s.partitions()))
	}
	var cand []volume.Partition
	for _, p := range s.partitions() {
		if _, ok := detect.ProbeWith(s.drivers(), s.section(p), p.Length); ok {
			cand = append(cand, p)
		}
	}
	switch len(cand) {
	case 1:
		return cand[0], nil
	case 0:
		return volume.Partition{}, fmt.Errorf("no partition holds a recognized filesystem (partitions: %s)", indexList(s.partitions()))
	default:
		return volume.Partition{}, fmt.Errorf("%d partitions hold a recognized filesystem (%s); choose one with --partition", len(cand), indexList(cand))
	}
}

func indexList(ps []volume.Partition) string {
	if len(ps) == 0 {
		return "none"
	}
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = strconv.Itoa(p.Index)
	}
	return strings.Join(parts, ", ")
}

// FS opens (and caches) the filesystem of partition index (same -1 rule as
// Partition). Every method of the returned FileSystem and of the Files it
// opens recovers parser panics into *filesys.CorruptError.
func (s *Session) FS(index int) (filesys.FileSystem, volume.Partition, error) {
	p, err := s.Partition(index)
	if err != nil {
		return nil, volume.Partition{}, err
	}
	fsys, err := s.openFS(p)
	if err != nil {
		return nil, p, err
	}
	return fsys, p, nil
}

func (s *Session) openFS(p volume.Partition) (filesys.FileSystem, error) {
	e := s.openEntry(p)
	return e.fs, e.err
}

// openEntry opens (and caches) the filesystem of p with detect.OpenWith, one
// driver at a time. detect stops at a driver whose Probe panics and reports it
// as a *filesys.CorruptError; here the panic is noted and the remaining
// drivers are still tried, so one faulty probe cannot hide a filesystem a
// later driver recognizes. Drivers are told apart by position, never by name,
// so duplicate names cannot re-run a driver or repeat a note. The first
// driver that matches is final, except that a matching driver whose Open fails
// with an error wrapping filesys.ErrCorrupt does not stop the search (as in
// detect.OpenWith): the later matching drivers are tried, the failure is kept
// as a note ("driver X matched but failed to open: ...; opened as Y") and, when
// no driver opens, the first failure is the result. Any other error is final.
// The notes are kept for Info.
func (s *Session) openEntry(p volume.Partition) *fsEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.fsCache[p.Index]; ok {
		return e
	}
	e := &fsEntry{}
	r := s.section(p)
	var firstFailed string // name of the first driver whose Open failed as corrupt
	var firstErr error
	var failed []string
	for _, d := range s.drivers() {
		one := []detect.Driver{d}
		fsys, err := detect.OpenWith(one, r, p.Length)
		var ce *filesys.CorruptError
		if errors.As(err, &ce) && strings.HasPrefix(ce.Reason, probePanicPrefix) {
			e.notes = append(e.notes, fmt.Sprintf("%s probe panicked: %s", d.Name, strings.TrimPrefix(ce.Reason, probePanicPrefix)))
			continue
		}
		if err != nil && errors.Is(err, filesys.ErrUnsupported) {
			if _, matched := detect.ProbeWith(one, r, p.Length); !matched {
				continue // no match; an Open that itself reports "unsupported" is a match
			}
		}
		if err != nil && errors.Is(err, filesys.ErrCorrupt) {
			if firstErr == nil {
				firstFailed, firstErr = d.Name, err
			}
			failed = append(failed, fmt.Sprintf("driver %s matched but failed to open: %v", d.Name, err))
			continue
		}
		e.name, e.fs, e.err = d.Name, fsys, err
		break
	}
	if e.name == "" && firstErr != nil {
		e.name, e.err = firstFailed, firstErr // nothing opened: report the first failure
		failed = failed[1:]                   // it is the error itself, not a note
	}
	for _, f := range failed {
		if e.err == nil {
			f += "; opened as " + e.name
		}
		e.notes = append(e.notes, f)
	}
	switch {
	case e.name == "":
		e.err = fmt.Errorf("%w: no recognized filesystem", filesys.ErrUnsupported)
		if len(e.notes) > 0 {
			e.err = fmt.Errorf("%w; %s", e.err, strings.Join(e.notes, "; "))
		}
	case e.err == nil:
		e.fs, e.err = wrapFS(fmt.Sprintf("partition %d filesystem", p.Index), e.fs)
	}
	s.fsCache[p.Index] = e
	return e
}

// probePanicPrefix starts the Reason detect gives a panicking Probe.
const probePanicPrefix = "probe panic: "

// PartitionInfo describes one partition and the filesystem found on it.
type PartitionInfo struct {
	Partition volume.Partition
	FSType    string        // "" when unrecognized
	FSInfo    *filesys.Info // nil when unrecognized
	Error     string        // probe/open error text, if any
}

// ImageInfo summarizes an image for display.
type ImageInfo struct {
	ParentID, Path, SHA256 string
	Incomplete             bool
	Format                 string
	Size                   int64
	SectorSize             int
	Metadata               []image.KV
	Scheme                 string
	DiskGUID               string
	Partitions             []PartitionInfo
	Unallocated            []volume.Run
	Warnings               []string
	// PartitionError is why the partition table could not be read (Table is
	// nil then and Scheme, Partitions and Unallocated are empty).
	PartitionError string
}

// Info describes the image, its partition table and, per partition, the
// filesystem (or why none could be opened).
func (s *Session) Info() ImageInfo {
	info := ImageInfo{
		ParentID: s.Parent.ID, Path: s.Parent.Path, SHA256: s.Parent.SHA256,
		Format: s.Image.Format(), Size: s.Image.Size(), SectorSize: s.Image.SectorSize(),
		Metadata: s.Image.Metadata(),
	}
	if t := s.Table; t != nil {
		info.SectorSize, info.Scheme, info.DiskGUID = t.SectorSize, t.Scheme, t.DiskGUID
		info.Unallocated = append([]volume.Run(nil), t.Unallocated...)
		info.Warnings = append([]string(nil), t.Warnings...)
	} else if s.tableErr != nil {
		info.PartitionError = s.tableErr.Error()
	}
	if w, ok := s.Image.(image.Warner); ok {
		info.Warnings = append(info.Warnings, w.Warnings()...)
	}
	for _, seg := range s.Segments {
		if seg.Incomplete {
			info.Incomplete = true
			info.Warnings = append(info.Warnings, fmt.Sprintf("artifact %s is incomplete (acquisition was interrupted)", seg.ID))
		}
	}
	for _, p := range s.partitions() {
		pi := PartitionInfo{Partition: p}
		e := s.openEntry(p)
		switch {
		case e.err == nil:
			fi := e.fs.Info()
			pi.FSInfo = &fi
			pi.FSType = fi.Type
			pi.Error = strings.Join(e.notes, "; ") // earlier drivers' probe panics
		case e.name == "":
			pi.Error = strings.Join(append([]string{"no recognized filesystem"}, e.notes...), "; ")
		default:
			pi.FSType = e.name
			pi.Error = strings.Join(append([]string{e.err.Error()}, e.notes...), "; ")
		}
		info.Partitions = append(info.Partitions, pi)
	}
	return info
}

var errFound = errors.New("found")

// Lookup resolves "/a/b" or "id:<fs id>" (searching the whole tree by ID, live
// and deleted entries alike, via filesys.Walk) and returns the entry with its
// slash path. When several entries share an id a live one is preferred over a
// deleted one. When the id is not found but some directories could not be read
// (or a directory cycle was hit) the error says so, since the entry may be in
// one of them.
func (s *Session) Lookup(fsys filesys.FileSystem, ref string) (filesys.Entry, string, error) {
	id, byID := strings.CutPrefix(ref, "id:")
	if !byID {
		p := path.Clean("/" + ref)
		e, err := fsys.Lookup(p)
		if err != nil {
			return filesys.Entry{}, "", fmt.Errorf("%s: %w", p, err)
		}
		return e, p, nil
	}
	if id == "" {
		return filesys.Entry{}, "", errors.New("empty entry id after \"id:\"")
	}
	root := fsys.Root()
	if root.ID == id {
		return root, "/", nil
	}
	var (
		found      filesys.Entry
		where      string
		haveMatch  bool
		unreadable int
		firstBad   string
	)
	err := filesys.Walk(fsys, root, "/", func(p string, e filesys.Entry, werr error) error {
		if werr != nil {
			if unreadable++; unreadable == 1 {
				firstBad = fmt.Sprintf("%s: %v", p, werr)
			}
			return nil
		}
		if e.ID != id {
			return nil
		}
		if !haveMatch || (found.Deleted && !e.Deleted) {
			found, where, haveMatch = e, p, true
		}
		if !found.Deleted {
			return errFound
		}
		return nil
	})
	switch {
	case err != nil && !errors.Is(err, errFound):
		return filesys.Entry{}, "", err
	case haveMatch:
		return found, where, nil
	case unreadable > 0:
		return filesys.Entry{}, "", fmt.Errorf("id:%s: %w; %d directories could not be read (first: %s)", id, filesys.ErrNotFound, unreadable, firstBad)
	}
	return filesys.Entry{}, "", fmt.Errorf("id:%s: %w", id, filesys.ErrNotFound)
}

// partitions lists the partitions of the table; none before ReadPartitions has
// succeeded.
func (s *Session) partitions() []volume.Partition {
	if s.Table == nil {
		return nil
	}
	return s.Table.Partitions
}

// needTable fails when the partition table has not been read: why not, if
// ReadPartitions failed.
func (s *Session) needTable() error {
	switch {
	case s.Table != nil:
		return nil
	case s.tableErr != nil:
		return s.tableErr
	}
	return errors.New("the partition table has not been read")
}
