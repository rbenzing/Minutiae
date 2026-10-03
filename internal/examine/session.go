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
	"sort"
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
	Table    *volume.Table

	opts      Options
	mu        sync.Mutex
	fsCache   map[int]*fsEntry
	closeOnce sync.Once
	closeErr  error
}

type fsEntry struct {
	fs  filesys.FileSystem
	err error
}

// Open resolves ref (artifact id or case-relative path), collects all segments
// of its import (same device id and acquisition directory, Source.Segment
// 1..N, contiguous; a gap or a missing file is evidence.ErrIntegrity), opens
// them read-only, opens the image and reads its partition table.
func Open(c *evidence.Case, ref string, opts Options) (*Session, error) {
	ref0, err := c.FindArtifact(ref)
	if err != nil {
		return nil, err
	}
	segs := []evidence.ManifestRecord{ref0}
	if ref0.Source.Segment > 0 {
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
	img, err := image.OpenFiles(files) // takes ownership of files, closing them on error
	if err != nil {
		return nil, fmt.Errorf("open image %s: %w", segs[0].Path, err)
	}
	// Raw containers report the default 512 without knowing it, so let the
	// partition reader probe 512 and 4096; a container that declares its sector
	// size (EWF) is trusted.
	sectorSize := img.SectorSize()
	if f := img.Format(); f == "raw" || f == "split-raw" {
		sectorSize = 0
	}
	tbl, err := volume.Read(img, img.Size(), sectorSize)
	if err != nil {
		_ = img.Close()
		return nil, fmt.Errorf("read partition table of %s: %w", segs[0].Path, err)
	}
	return &Session{
		Case: c, Parent: segs[0], Segments: segs, Image: img, Table: tbl,
		opts: opts, fsCache: map[int]*fsEntry{},
	}, nil
}

// collectSegments returns the records of the import that ref belongs to, in
// segment order. Records belong to one import when they carry a segment number
// and share the device id and the acquisition directory.
func collectSegments(recs []evidence.ManifestRecord, ref evidence.ManifestRecord) ([]evidence.ManifestRecord, error) {
	dir := path.Dir(ref.Path)
	var group []evidence.ManifestRecord
	for _, r := range recs {
		if r.Source.Segment > 0 && r.Source.DeviceID == ref.Source.DeviceID && path.Dir(r.Path) == dir {
			group = append(group, r)
		}
	}
	sort.SliceStable(group, func(i, j int) bool { return group[i].Source.Segment < group[j].Source.Segment })
	for i, r := range group {
		if r.Source.Segment != i+1 {
			return nil, fmt.Errorf("%w: image segments of %s are not contiguous 1..%d (found segment %d at position %d)",
				evidence.ErrIntegrity, dir, len(group), r.Source.Segment, i+1)
		}
	}
	return group, nil
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
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
	for _, p := range s.Table.Partitions {
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
	if index != -1 {
		if p, ok := s.partitionByIndex(index); ok {
			return p, nil
		}
		return volume.Partition{}, fmt.Errorf("no partition %d (have %s)", index, indexList(s.Table.Partitions))
	}
	var cand []volume.Partition
	for _, p := range s.Table.Partitions {
		if _, ok := detect.ProbeWith(s.drivers(), s.section(p), p.Length); ok {
			cand = append(cand, p)
		}
	}
	switch len(cand) {
	case 1:
		return cand[0], nil
	case 0:
		return volume.Partition{}, fmt.Errorf("no partition holds a recognized filesystem (partitions: %s)", indexList(s.Table.Partitions))
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.fsCache[p.Index]; ok {
		return e.fs, e.err
	}
	fsys, err := detect.OpenWith(s.drivers(), s.section(p), p.Length)
	if err == nil {
		name := fmt.Sprintf("partition %d filesystem", p.Index)
		fsys, err = wrapFS(name, fsys)
	}
	s.fsCache[p.Index] = &fsEntry{fs: fsys, err: err}
	return fsys, err
}

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
}

// Info describes the image, its partition table and, per partition, the
// filesystem (or why none could be opened).
func (s *Session) Info() ImageInfo {
	info := ImageInfo{
		ParentID: s.Parent.ID, Path: s.Parent.Path, SHA256: s.Parent.SHA256,
		Format: s.Image.Format(), Size: s.Image.Size(), SectorSize: s.Table.SectorSize,
		Metadata: s.Image.Metadata(), Scheme: s.Table.Scheme, DiskGUID: s.Table.DiskGUID,
		Unallocated: append([]volume.Run(nil), s.Table.Unallocated...),
		Warnings:    append([]string(nil), s.Table.Warnings...),
	}
	for _, seg := range s.Segments {
		if seg.Incomplete {
			info.Incomplete = true
			info.Warnings = append(info.Warnings, fmt.Sprintf("artifact %s is incomplete (acquisition was interrupted)", seg.ID))
		}
	}
	for _, p := range s.Table.Partitions {
		pi := PartitionInfo{Partition: p}
		name, recognized := detect.ProbeWith(s.drivers(), s.section(p), p.Length)
		if !recognized {
			pi.Error = "no recognized filesystem"
		} else {
			pi.FSType = name
			if fsys, err := s.openFS(p); err != nil {
				pi.Error = err.Error()
			} else {
				fi := fsys.Info()
				pi.FSInfo = &fi
				pi.FSType = fi.Type
			}
		}
		info.Partitions = append(info.Partitions, pi)
	}
	return info
}

var errFound = errors.New("found")

// Lookup resolves "/a/b" or "id:<fs id>" (searching the whole tree by ID, live
// and deleted entries alike, via filesys.Walk) and returns the entry with its
// slash path.
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
		found filesys.Entry
		where string
	)
	err := filesys.Walk(fsys, root, "/", func(p string, e filesys.Entry, werr error) error {
		if werr != nil || e.ID != id {
			return nil // unreadable directories are skipped; they cannot be the answer
		}
		found, where = e, p
		return errFound
	})
	switch {
	case errors.Is(err, errFound):
		return found, where, nil
	case err != nil:
		return filesys.Entry{}, "", err
	}
	return filesys.Entry{}, "", fmt.Errorf("id:%s: %w", id, filesys.ErrNotFound)
}
