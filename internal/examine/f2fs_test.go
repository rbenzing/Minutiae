package examine_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// f2fsFlat presents an F2FS image as a flat root directory of fixed entries.
// The directory reader of the F2FS driver is a separate piece of work; the
// file data reader under test here is the real one (Open, Runs, ReadAt).
type f2fsFlat struct {
	*f2fs.FS
	entries []filesys.Entry
}

func (a f2fsFlat) Root() filesys.Entry {
	return filesys.Entry{Name: "/", ID: "nid:3", Type: filesys.TypeDir}
}

func (a f2fsFlat) ReadDir(filesys.Entry) ([]filesys.Entry, error) { return a.entries, nil }

func (a f2fsFlat) Lookup(path string) (filesys.Entry, error) {
	if path == "/" {
		return a.Root(), nil
	}
	for _, e := range a.entries {
		if "/"+e.Name == path {
			return e, nil
		}
	}
	return filesys.Entry{}, filesys.ErrNotFound
}

func (a f2fsFlat) Unallocated() ([]filesys.Run, error) { return nil, nil }

// A file whose block map breaks part-way (a data address outside the main
// area) is extracted as a partial artifact flagged incomplete: the runs of the
// trusted prefix are recorded and reproduce exactly the captured bytes, the
// intact neighbour is unaffected, and the filesystem's own warning is counted
// separately from the skip.
func TestExtractF2FSBrokenChainIsIncomplete(t *testing.T) {
	const bs = f2fstest.BlockSize
	o := f2fstest.Options{Segments: 1}
	content := pattern(6*bs, 3)
	a := f2fstest.NewAlloc(o, 10)
	nodes, data := a.File(o, f2fstest.Inode{NID: 5, Mode: 0o100644, Size: uint64(len(content))}, f2fstest.FileData{Blocks: map[int64][]byte{
		0: content[0:bs], 1: content[bs : 2*bs], 2: content[2*bs : 3*bs], 3: content[3*bs : 4*bs], 4: content[4*bs : 5*bs], 5: content[5*bs:],
	}})
	binary.LittleEndian.PutUint32(nodes[0].Block[360+3*4:], 1) // i_addr[3]: block 1, below the main area
	nodes = append(nodes, f2fstest.Node{NID: 6, Addr: a.Addr(), Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: 6, Mode: 0o100644, Size: 4, InlineData: []byte("fine")})})
	o.Nodes, o.Data = nodes, data
	img := f2fstest.Build(o, nil)

	c := newCase(t)
	disc := disk(img)
	recs := importImage(t, c, disc, 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: []detect.Driver{{
		Name: "f2fs", Probe: f2fs.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := f2fs.Open(r, size)
			if err != nil {
				return nil, err
			}
			return f2fsFlat{FS: fsys, entries: []filesys.Entry{
				{Name: "BROKEN.BIN", ID: "nid:5", Type: filesys.TypeFile, Size: int64(len(content))},
				{Name: "FINE.TXT", ID: "nid:6", Type: filesys.TypeFile, Size: 4},
			}}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})

	if sum.Files != 1 || len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	broken, fine := sum.Artifacts[0], sum.Artifacts[1]
	if strings.HasSuffix(broken.Source.RemotePath, "FINE.TXT") {
		broken, fine = fine, broken
	}
	if !strings.HasSuffix(broken.Source.RemotePath, "BROKEN.BIN") {
		t.Fatalf("artifacts = %q / %q", broken.Source.RemotePath, fine.Source.RemotePath)
	}
	if !strings.Contains(broken.Path, "/p1-f2fs/") {
		t.Errorf("path %q does not name the filesystem type", broken.Path)
	}
	if !broken.Incomplete || broken.Size != 3*bs || !bytes.Equal(readArtifact(t, c, broken), content[:3*bs]) {
		t.Errorf("broken artifact: incomplete=%v size=%d, want an incomplete 3-block prefix", broken.Incomplete, broken.Size)
	}
	d := broken.Source.Derived
	if d == nil || len(d.Runs) == 0 || d.RunsArtifact != "" {
		t.Fatalf("the prefix runs were not recorded: %+v", d)
	}
	var fromRuns []byte
	var covered int64
	for _, r := range d.Runs {
		if r.Offset < 0 || r.Offset+r.Length > int64(len(disc)) {
			t.Fatalf("run %+v is not inside the %d-byte image", r, len(disc))
		}
		fromRuns = append(fromRuns, disc[r.Offset:r.Offset+r.Length]...)
		covered += r.Length
	}
	if covered != broken.Size || !bytes.Equal(fromRuns, readArtifact(t, c, broken)) {
		t.Errorf("recorded runs cover %d bytes and do not reproduce the %d captured bytes", covered, broken.Size)
	}
	if fine.Incomplete || string(readArtifact(t, c, fine)) != "fine" {
		t.Errorf("the intact neighbour was affected: %+v", fine)
	}
	if sum.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (read failed)", sum.Skipped)
	}
	if sum.FSWarnings != 1 {
		t.Errorf("FSWarnings = %d, want 1 (the block map is broken)", sum.FSWarnings)
	}
	var reasons []string
	for _, e := range auditByAction(t, c, "analysis.warning") {
		if r, _ := e.Details["reason"].(string); r != "" {
			reasons = append(reasons, r)
		}
	}
	joined := strings.Join(reasons, "\n")
	if strings.Contains(joined, "no runs recorded") || !strings.Contains(joined, "partial artifact kept, flagged incomplete") {
		t.Errorf("warnings do not explain the partial extraction: %q", reasons)
	}
	verifyOK(t, c)
}
