// Package fstest provides MTFS, a deliberately simple test filesystem, so that
// the image-examination code can be tested end to end against real image
// bytes without a real filesystem parser. Build lays out an image, Probe and
// Open read it back through the filesys interfaces, and Encode assembles
// hand-crafted (including hostile) images.
//
// MTFS layout:
//
//	bytes 0-7    magic "MTFS0001"
//	bytes 8-15   little-endian uint64 T, the length of the JSON table
//	16..16+T     JSON table
//	then         the data area (block-aligned relative to the filesystem start)
//
// The table is {"label":..,"block_size":512,"entries":[{"id","parent_id",
// "name","type","size","mode","uid","gid","mtime","deleted","encrypted",
// "link","runs":[{"offset","length"}]}]}. Run offsets are relative to the
// filesystem start; offset -1 is a sparse hole. The root has id "1" and an
// empty parent_id.
package fstest

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	magic         = "MTFS0001"
	headerLen     = 16
	maxTableLen   = 16 << 20
	maxBlockSize  = 1 << 20
	rootID        = "1"
	structHeader  = "mtfs header"
	structTable   = "mtfs table"
	structEntry   = "mtfs entry"
	typeName      = "mtfs"
	errNotRegular = "not a regular file or symlink"
)

type table struct {
	Label     string   `json:"label"`
	BlockSize int64    `json:"block_size"`
	Entries   []record `json:"entries"`
}

type record struct {
	ID        string `json:"id"`
	ParentID  string `json:"parent_id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Size      int64  `json:"size"`
	Mode      uint32 `json:"mode,omitempty"`
	UID       uint32 `json:"uid,omitempty"`
	GID       uint32 `json:"gid,omitempty"`
	MTime     int64  `json:"mtime,omitempty"`
	Deleted   bool   `json:"deleted,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
	Link      string `json:"link,omitempty"`
	Runs      []run  `json:"runs,omitempty"`
}

type run struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// Encode assembles a raw MTFS image from table JSON and data-area bytes. It
// validates and aligns nothing: it exists for hand-crafted and hostile images.
// Use Build for well-formed ones.
func Encode(tableJSON, data []byte) []byte {
	out := make([]byte, 0, headerLen+len(tableJSON)+len(data))
	out = append(out, magic...)
	out = binary.LittleEndian.AppendUint64(out, uint64(len(tableJSON)))
	out = append(out, tableJSON...)
	return append(out, data...)
}

// Probe reports whether the filesystem at the start of r carries the MTFS
// magic.
func Probe(r io.ReaderAt, size int64) bool {
	if size < headerLen {
		return false
	}
	var m [len(magic)]byte
	n, err := r.ReadAt(m[:], 0)
	return n == len(m) && (err == nil || errors.Is(err, io.EOF)) && string(m[:]) == magic
}

func corrupt(structure string, off int64, format string, a ...any) error {
	return &filesys.CorruptError{Structure: structure, Offset: off, Reason: fmt.Sprintf(format, a...)}
}

type mtfs struct {
	r         io.ReaderAt
	size      int64
	bs        int64
	label     string
	recs      []record
	byID      map[string][]int // record indexes by id (duplicates are kept: hostile images)
	byParent  map[string][]int // child record indexes by parent id, sorted by (name, id)
	dataStart int64
	encrypted bool

	unallocOnce sync.Once
	unalloc     []filesys.Run
}

// Open parses an MTFS filesystem of size bytes. Structural problems are
// reported as *filesys.CorruptError.
func Open(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	if size < headerLen {
		return nil, corrupt(structHeader, 0, "image of %d bytes is shorter than the %d-byte header", size, headerLen)
	}
	var hdr [headerLen]byte
	if n, err := r.ReadAt(hdr[:], 0); n < headerLen {
		if err == nil || errors.Is(err, io.EOF) {
			return nil, corrupt(structHeader, 0, "short read of the header")
		}
		return nil, err
	}
	if string(hdr[:len(magic)]) != magic {
		return nil, corrupt(structHeader, 0, "bad magic %q", hdr[:len(magic)])
	}
	tlen := binary.LittleEndian.Uint64(hdr[8:])
	if tlen > maxTableLen {
		return nil, corrupt(structHeader, 8, "table length %d exceeds the %d byte limit", tlen, maxTableLen)
	}
	if tlen > uint64(size-headerLen) {
		return nil, corrupt(structHeader, 8, "table length %d exceeds the filesystem size %d", tlen, size)
	}
	raw := make([]byte, tlen)
	if n, err := r.ReadAt(raw, headerLen); n < len(raw) {
		if err == nil || errors.Is(err, io.EOF) {
			return nil, corrupt(structTable, headerLen, "short read of the table")
		}
		return nil, err
	}
	var tbl table
	if err := json.Unmarshal(raw, &tbl); err != nil {
		return nil, corrupt(structTable, headerLen, "bad table JSON: %v", err)
	}
	f := &mtfs{r: r, size: size, bs: tbl.BlockSize, label: tbl.Label, recs: tbl.Entries}
	if err := f.validate(); err != nil {
		return nil, err
	}
	f.index(int64(tlen))
	return f, nil
}

func typeOf(s string) (filesys.EntryType, bool) {
	switch s {
	case "file":
		return filesys.TypeFile, true
	case "dir":
		return filesys.TypeDir, true
	case "symlink":
		return filesys.TypeSymlink, true
	case "other":
		return filesys.TypeOther, true
	}
	return filesys.TypeOther, false
}

func (f *mtfs) validate() error {
	if f.bs < 1 || f.bs > maxBlockSize {
		return corrupt(structTable, headerLen, "block size %d outside 1..%d", f.bs, maxBlockSize)
	}
	haveRoot := false
	for i := range f.recs {
		rec := &f.recs[i]
		if rec.ID == "" {
			return corrupt(structEntry, headerLen, "entry %d has an empty id", i)
		}
		typ, ok := typeOf(rec.Type)
		if !ok {
			return corrupt(structEntry, headerLen, "entry %q has unknown type %q", rec.ID, rec.Type)
		}
		if rec.Size < 0 {
			return corrupt(structEntry, headerLen, "entry %q has negative size %d", rec.ID, rec.Size)
		}
		if rec.ID == rootID && rec.ParentID == "" {
			if typ != filesys.TypeDir {
				return corrupt(structEntry, headerLen, "root entry is a %s, not a dir", rec.Type)
			}
			haveRoot = true
		} else if rec.Name == "" || rec.Name == "." || rec.Name == ".." || strings.ContainsAny(rec.Name, "/\x00") {
			return corrupt(structEntry, headerLen, "entry %q has invalid name %q", rec.ID, rec.Name)
		}
		for _, ru := range rec.Runs {
			if err := f.checkRun(rec.ID, ru); err != nil {
				return err
			}
		}
	}
	if !haveRoot {
		return corrupt(structTable, headerLen, "no root entry (id %q, empty parent)", rootID)
	}
	return nil
}

func (f *mtfs) checkRun(id string, ru run) error {
	if ru.Length <= 0 {
		return corrupt(structEntry, headerLen, "entry %q has a run of length %d", id, ru.Length)
	}
	if ru.Offset == -1 {
		return nil // hole
	}
	if ru.Offset < 0 {
		return corrupt(structEntry, headerLen, "entry %q has a run at negative offset %d", id, ru.Offset)
	}
	if end, ok := filesys.AddOK(ru.Offset, ru.Length); !ok || end > f.size {
		return corrupt(structEntry, headerLen, "entry %q has run %d+%d outside the %d-byte filesystem", id, ru.Offset, ru.Length, f.size)
	}
	return nil
}

func (f *mtfs) index(tlen int64) {
	f.byID = make(map[string][]int, len(f.recs))
	f.byParent = make(map[string][]int, len(f.recs))
	for i, rec := range f.recs {
		f.byID[rec.ID] = append(f.byID[rec.ID], i)
		f.encrypted = f.encrypted || rec.Encrypted
		if rec.ID == rootID && rec.ParentID == "" {
			continue
		}
		f.byParent[rec.ParentID] = append(f.byParent[rec.ParentID], i)
	}
	for _, kids := range f.byParent {
		sort.SliceStable(kids, func(a, b int) bool {
			ra, rb := f.recs[kids[a]], f.recs[kids[b]]
			if ra.Name != rb.Name {
				return ra.Name < rb.Name
			}
			return ra.ID < rb.ID
		})
	}
	start := headerLen + tlen // validated: <= size
	if rem := start % f.bs; rem != 0 {
		start += f.bs - rem
	}
	f.dataStart = min(start, f.size)
}

func (f *mtfs) rootIndex() int {
	for _, i := range f.byID[rootID] {
		if f.recs[i].ParentID == "" {
			return i
		}
	}
	return 0 // unreachable: validate guarantees a root
}

func (f *mtfs) entry(rec *record) filesys.Entry {
	typ, _ := typeOf(rec.Type)
	e := filesys.Entry{
		Name:       rec.Name,
		ID:         rec.ID,
		Type:       typ,
		Size:       rec.Size,
		Mode:       rec.Mode,
		UID:        rec.UID,
		GID:        rec.GID,
		Deleted:    rec.Deleted,
		Encrypted:  rec.Encrypted,
		LinkTarget: rec.Link,
	}
	if rec.MTime != 0 {
		e.Times.Modified = filesys.Timestamp{T: time.Unix(rec.MTime, 0).UTC(), ZoneKnown: true}
	}
	return e
}

// Info implements filesys.FileSystem.
func (f *mtfs) Info() filesys.Info {
	return filesys.Info{Type: typeName, Label: f.label, BlockSize: int(f.bs), Size: f.size, Encrypted: f.encrypted}
}

// Root implements filesys.FileSystem.
func (f *mtfs) Root() filesys.Entry { return f.entry(&f.recs[f.rootIndex()]) }

// ReadDir implements filesys.FileSystem.
func (f *mtfs) ReadDir(dir filesys.Entry) ([]filesys.Entry, error) {
	rec := f.find(dir)
	if rec == nil {
		return nil, fmt.Errorf("%w: directory %q", filesys.ErrNotFound, dir.ID)
	}
	if typ, _ := typeOf(rec.Type); typ != filesys.TypeDir {
		return nil, fmt.Errorf("%w: %q is not a directory", filesys.ErrUnsupported, dir.Name)
	}
	kids := f.byParent[rec.ID]
	out := make([]filesys.Entry, 0, len(kids))
	for _, i := range kids {
		out = append(out, f.entry(&f.recs[i]))
	}
	return out, nil
}

// find returns the table record for e: matching id and, among duplicate ids
// (hostile images only), the one with the same name.
func (f *mtfs) find(e filesys.Entry) *record {
	idx := f.byID[e.ID]
	if len(idx) == 0 {
		return nil
	}
	for _, i := range idx {
		if f.recs[i].Name == e.Name {
			return &f.recs[i]
		}
	}
	return &f.recs[idx[0]]
}

// Lookup implements filesys.FileSystem. Symlinks are not followed and deleted
// entries are ignored.
func (f *mtfs) Lookup(p string) (filesys.Entry, error) {
	cur := &f.recs[f.rootIndex()]
	for _, name := range strings.Split(p, "/") {
		if name == "" {
			continue
		}
		if typ, _ := typeOf(cur.Type); typ != filesys.TypeDir {
			return filesys.Entry{}, fmt.Errorf("%w: %s", filesys.ErrNotFound, p)
		}
		var next *record
		for _, i := range f.byParent[cur.ID] {
			if k := &f.recs[i]; k.Name == name && !k.Deleted {
				next = k
				break
			}
		}
		if next == nil {
			return filesys.Entry{}, fmt.Errorf("%w: %s", filesys.ErrNotFound, p)
		}
		cur = next
	}
	return f.entry(cur), nil
}

// Open implements filesys.FileSystem.
func (f *mtfs) Open(e filesys.Entry) (filesys.File, error) {
	rec := f.find(e)
	if rec == nil {
		return nil, fmt.Errorf("%w: %q", filesys.ErrNotFound, e.ID)
	}
	if rec.Deleted || e.Deleted {
		return nil, fmt.Errorf("%w: %q", filesys.ErrDeleted, rec.Name)
	}
	switch typ, _ := typeOf(rec.Type); typ {
	case filesys.TypeSymlink:
		return &file{data: []byte(rec.Link), size: int64(len(rec.Link))}, nil
	case filesys.TypeFile:
		return f.openFile(rec)
	default:
		return nil, fmt.Errorf("%w: %q is a %s: %s", filesys.ErrUnsupported, rec.Name, rec.Type, errNotRegular)
	}
}

func (f *mtfs) openFile(rec *record) (*file, error) {
	fl := &file{r: f.r, size: rec.Size}
	var total int64
	for _, ru := range rec.Runs {
		if total >= rec.Size {
			break // the table's block slack beyond Size is not file content
		}
		length := min(ru.Length, rec.Size-total) // trim the last run so the runs cover exactly [0, Size)
		fl.runs = append(fl.runs, filesys.Run{Offset: ru.Offset, Length: length})
		fl.starts = append(fl.starts, total)
		total += length // cannot overflow: total+length <= rec.Size
	}
	if total < rec.Size {
		return nil, corrupt(structEntry, headerLen, "entry %q: size %d exceeds its runs (%d bytes)", rec.ID, rec.Size, total)
	}
	return fl, nil
}

// Unallocated implements filesys.FileSystem: the block-aligned data area
// minus every run of every entry (deleted entries included).
func (f *mtfs) Unallocated() ([]filesys.Run, error) {
	f.unallocOnce.Do(func() {
		var used []filesys.Run
		for i := range f.recs {
			for _, ru := range f.recs[i].Runs {
				if ru.Offset >= 0 {
					used = append(used, filesys.Run{Offset: ru.Offset, Length: ru.Length})
				}
			}
		}
		dataEnd := f.size - f.size%f.bs
		cur := f.dataStart
		for _, u := range filesys.MergeRuns(used) {
			if u.Offset+u.Length <= cur {
				continue
			}
			if gapEnd := min(u.Offset, dataEnd); gapEnd > cur {
				f.unalloc = append(f.unalloc, filesys.Run{Offset: cur, Length: gapEnd - cur})
			}
			cur = max(cur, u.Offset+u.Length)
		}
		if dataEnd > cur {
			f.unalloc = append(f.unalloc, filesys.Run{Offset: cur, Length: dataEnd - cur})
		}
	})
	return append([]filesys.Run(nil), f.unalloc...), nil
}

// file is an open MTFS file or symlink.
type file struct {
	r      io.ReaderAt
	size   int64
	runs   []filesys.Run
	starts []int64 // logical byte offset where each run begins
	data   []byte  // symlink target (no runs)
}

func (fl *file) Size() int64 { return fl.size }

func (fl *file) Runs() []filesys.Run { return append([]filesys.Run(nil), fl.runs...) }

func (fl *file) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("mtfs: negative read offset")
	}
	if off >= fl.size {
		return 0, io.EOF
	}
	want := min(int64(len(p)), fl.size-off)
	if fl.data != nil {
		return finish(copy(p[:want], fl.data[off:]), len(p))
	}
	i := sort.Search(len(fl.runs), func(i int) bool { return fl.starts[i]+fl.runs[i].Length > off })
	var n int64
	for n < want && i < len(fl.runs) {
		ru := fl.runs[i]
		in := off + n - fl.starts[i]
		chunk := min(ru.Length-in, want-n)
		dst := p[n : n+chunk]
		if ru.Offset < 0 {
			clear(dst)
		} else if m, err := fl.r.ReadAt(dst, ru.Offset+in); m < len(dst) {
			if err == nil || errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return int(n) + m, err
		}
		n += chunk
		i++
	}
	if n < want {
		return int(n), io.ErrUnexpectedEOF
	}
	return finish(int(n), len(p))
}

func finish(n, wanted int) (int, error) {
	if n < wanted {
		return n, io.EOF
	}
	return n, nil
}
