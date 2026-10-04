package examine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

const e01Chunk = 8 * 512 // 8 sectors per chunk

// e01ChunkSession builds a GPT disk around an MTFS image holding the nodes,
// wraps it in a 3-segment E01 set, flips one byte of the stored chunk holding
// corruptAt (a disk offset; negative = no corruption), imports the set and
// opens it.
func e01ChunkSession(t *testing.T, c *evidence.Case, nodes []fstest.Node, corruptAt int64) (*examine.Session, []byte) {
	t.Helper()
	return e01ChunkSessionOpts(t, c, nodes, corruptAt, mtfsOpts())
}

// e01ChunkSessionOpts is e01ChunkSession with the session options given.
func e01ChunkSessionOpts(t *testing.T, c *evidence.Case, nodes []fstest.Node, corruptAt int64, opts examine.Options) (*examine.Session, []byte) {
	t.Helper()
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes}))
	o := ewftest.Options{SectorsPerChunk: 8, ChunksPerSegment: (len(data)/e01Chunk + 2) / 3, Compress: ewftest.CompressNone}
	segs := ewftest.Build(o, data)
	if corruptAt >= 0 {
		l := ewftest.ChunkLocs(segs)[corruptAt/e01Chunk]
		segs[l.Segment-1][l.Offset+l.Length/2] ^= 0x5A
	}
	dir := t.TempDir()
	paths := make([]string, len(segs))
	for i, s := range segs {
		paths[i] = filepath.Join(dir, "disk.E0"+string(rune('1'+i)))
		if err := os.WriteFile(paths[i], s, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := examine.Import(context.Background(), c, "img", paths, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	s, err := examine.Open(c, recs[0].ID, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, data
}

// A chunk that cannot be decoded in the middle of one file makes that file an
// incomplete artifact (the bytes before the chunk, never zeros), with the
// provenance runs of that prefix only; the run goes on with the next files.
func TestExtractUnreadableChunkMakesFileIncompleteAndContinues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		frags    int  // > MaxInlineRuns forces a full runs sidecar
		badBlock int  // block of the file whose chunk is corrupted; 0 = near the end
		collide  bool // the prefix sidecar names are taken by other files
	}{
		{"inline runs", 0, 0, false},
		{"sidecar runs", evidence.MaxInlineRuns + 2000, 0, false},
		{"full sidecar and an inline prefix", evidence.MaxInlineRuns + 2000, 40, false},
		{"prefix sidecar name collision", evidence.MaxInlineRuns + 2000, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size := 6*e01Chunk + 100
			if tc.frags > 0 {
				size = tc.frags * blk
			}
			big := pattern(size, 3)
			nodes := []fstest.Node{
				{Path: "/a-first.txt", Data: []byte("first")},
				{Path: "/big.bin", Data: big, Fragments: tc.frags},
				{Path: "/z-last.txt", Data: []byte("last file")},
			}
			ref := fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})
			base := bytes.Index(disk(ref), big[:64]) // fragments are laid out in order from here
			if base < 0 {
				t.Fatal("cannot find the file data in the disk")
			}
			stride := blk // fragments are one block apart with a free block between them
			corruptAt := int64(base + size/2)
			if tc.frags > 0 {
				stride = 2 * blk
				bb := tc.badBlock
				if bb == 0 {
					bb = tc.frags - 200
				}
				corruptAt = int64(base + bb*stride)
			}
			c := newCase(t)
			opts := mtfsOpts()
			if tc.collide {
				opts = squatPrefixSidecarNames(t, c, "big.bin", 2)
			}
			s, data := e01ChunkSessionOpts(t, c, nodes, corruptAt, opts)
			chunkStart := corruptAt / e01Chunk * e01Chunk
			end := base + size
			if tc.frags > 0 {
				end = base + (tc.frags-1)*stride + blk
			}
			if chunkStart <= int64(base) || chunkStart+e01Chunk >= int64(end) {
				t.Fatalf("test setup: chunk %d..%d is not strictly inside the file at %d..%d", chunkStart, chunkStart+e01Chunk, base, end)
			}
			for i := range size / blk { // the layout this test assumes
				if !bytes.Equal(data[base+i*stride:base+i*stride+blk], big[i*blk:(i+1)*blk]) {
					t.Fatalf("fragment %d is not where the test expects", i)
				}
			}
			sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})

			byPath := map[string]evidence.ManifestRecord{}
			for _, r := range sum.Artifacts {
				byPath[r.Source.RemotePath] = r
			}
			if string(readArtifact(t, c, byPath["/a-first.txt"])) != "first" || string(readArtifact(t, c, byPath["/z-last.txt"])) != "last file" ||
				byPath["/a-first.txt"].Incomplete || byPath["/z-last.txt"].Incomplete {
				t.Fatalf("the neighbours were affected: %+v", sum.Artifacts)
			}
			rec := byPath["/big.bin"]
			var want []byte // the blocks that lie completely before the bad chunk
			for i := 0; (i+1)*blk <= len(big) && int64(base+i*stride+blk) <= chunkStart; i++ {
				want = append(want, big[i*blk:(i+1)*blk]...)
			}
			got := readArtifact(t, c, rec)
			if !rec.Incomplete || !bytes.Equal(got, want) || rec.Error == "" || !strings.Contains(rec.Error, "chunk") {
				t.Fatalf("big.bin: incomplete=%v size=%d (want prefix of %d) error=%q", rec.Incomplete, rec.Size, len(want), rec.Error)
			}
			if sum.Files != 2 || sum.Skipped != 1 {
				t.Errorf("summary Files=%d Skipped=%d, want 2 and 1", sum.Files, sum.Skipped)
			}
			d := rec.Source.Derived
			runs := d.Runs
			prefixBlocks := len(want) / blk
			switch {
			case tc.frags > 0 && prefixBlocks > evidence.MaxInlineRuns:
				// A prefix this long still needs a sidecar; it must hold the prefix runs only.
				if len(runs) != 0 || d.RunsArtifact == "" {
					t.Fatalf("runs %d, sidecar %q", len(runs), d.RunsArtifact)
				}
				runs = readSidecarRuns(t, c, d.RunsArtifact)
				if tc.collide && !strings.HasSuffix(sidecarPath(t, c, d.RunsArtifact), "/big.bin.incomplete~3.runs.jsonl") {
					t.Errorf("prefix sidecar is %q, want the third candidate name", sidecarPath(t, c, d.RunsArtifact))
				}
			case tc.frags > 0:
				if len(runs) == 0 || d.RunsArtifact != "" {
					t.Fatalf("a short prefix must be inline: runs %d, sidecar %q", len(runs), d.RunsArtifact)
				}
			}
			if tc.frags > 0 {
				// The full run list written before the read stays in the case; the
				// partial artifact says which artifact it is.
				full := fullRunsArtifact(t, c, d.RunsArtifact)
				if !strings.Contains(rec.Error, full) {
					t.Errorf("rec.Error %q does not name the full runs artifact %s", rec.Error, full)
				}
				if full == d.RunsArtifact {
					t.Error("the partial artifact references the full-run sidecar")
				}
			}
			var covered int64
			for _, r := range runs {
				covered += r.Length
			}
			if covered != rec.Size || !bytes.Equal(readImageRuns(data, runs), got) {
				t.Errorf("recorded runs cover %d bytes and do not reproduce the %d captured bytes", covered, rec.Size)
			}
			var reasons []string
			for _, e := range auditByAction(t, c, "analysis.warning") {
				r, _ := e.Details["reason"].(string)
				reasons = append(reasons, r)
			}
			if joined := strings.Join(reasons, "\n"); !strings.Contains(joined, "partial artifact kept, flagged incomplete") {
				t.Errorf("warnings: %q", reasons)
			}
			if len(auditByAction(t, c, "analysis.end")) != 1 || len(auditByAction(t, c, "analysis.error")) != 0 {
				t.Error("the analysis must end normally")
			}
			if !tc.collide {
				verifyOK(t, c)
				return
			}
			// The squatting files the test planted are the only problem of the case.
			rep, err := c.Verify()
			if err != nil || len(rep.Problems) != 2 {
				t.Fatalf("case verify: %v, %v", rep.Problems, err)
			}
			for _, p := range rep.Problems {
				if !strings.Contains(p, "unmanifested file") || !strings.Contains(p, "big.bin.incomplete") {
					t.Errorf("unexpected problem %q", p)
				}
			}
		})
	}
}

// A failure to write to the case is still never downgraded, even while reading
// a damaged image.
func TestExtractUnreadableChunkDoesNotMaskCaseWriteFailure(t *testing.T) {
	big := pattern(6*e01Chunk, 4)
	nodes := []fstest.Node{{Path: "/big.bin", Data: big}}
	base := bytes.Index(disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})), big[:64])
	c := newCase(t)
	s, _ := e01ChunkSession(t, c, nodes, int64(base+len(big)/2))
	s.SetNewArtifact(func(string, string, string, evidence.Source) (*evidence.ArtifactWriter, error) {
		return nil, errors.New("disk full")
	})
	_, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"/big.bin"}})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
}

// readSidecarRuns reads a runs sidecar artifact back.
func readSidecarRuns(t *testing.T, c *evidence.Case, id string) []evidence.Run {
	t.Helper()
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID != id {
			continue
		}
		var runs []evidence.Run
		for _, line := range strings.Split(strings.TrimSuffix(string(readArtifact(t, c, r)), "\n"), "\n") {
			var run evidence.Run
			if err := json.Unmarshal([]byte(line), &run); err != nil {
				t.Fatal(err)
			}
			runs = append(runs, run)
		}
		return runs
	}
	t.Fatalf("runs artifact %s is not in the manifest", id)
	return nil
}

// sidecarPath returns the manifest path of artifact id.
func sidecarPath(t *testing.T, c *evidence.Case, id string) string {
	t.Helper()
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID == id {
			return r.Path
		}
	}
	t.Fatalf("artifact %s is not in the manifest", id)
	return ""
}

// fullRunsArtifact returns the id of the runs artifact of /big.bin that is not
// referenced by the partial artifact (the complete run list written before the
// read failed). prefixID may be empty (an inline prefix).
func fullRunsArtifact(t *testing.T, c *evidence.Case, prefixID string) string {
	t.Helper()
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, r := range recs {
		if r.Source.Kind == "runs" && r.Source.RemotePath == "/big.bin" && r.ID != prefixID && strings.HasSuffix(r.Path, "/big.bin.runs.jsonl") {
			found = append(found, r.ID)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d full runs artifacts for /big.bin, want 1", len(found))
	}
	return found[0]
}

// squatPrefixSidecarNames returns session options whose filesystem, when file
// name is opened, occupies the first n candidate names of its prefix runs
// sidecar (name.incomplete.runs.jsonl, name.incomplete~2.runs.jsonl ...) with
// other files, as an odd image or a later entry of the walk could.
func squatPrefixSidecarNames(t *testing.T, c *evidence.Case, name string, n int) examine.Options {
	t.Helper()
	hook := hookFS{mapFile: func(e filesys.Entry, f filesys.File) filesys.File {
		if e.Name != name {
			return f
		}
		starts := auditByAction(t, c, "analysis.start")
		acq := starts[len(starts)-1].Details["analysis_id"].(string)
		dir := filepath.Join(c.Dir, "artifacts", "img", acq, "p1-mtfs")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		cands := []string{name + ".incomplete.runs.jsonl"}
		for i := 2; i <= n; i++ {
			cands = append(cands, name+".incomplete~"+strconv.Itoa(i)+".runs.jsonl")
		}
		for _, cand := range cands[:n] {
			if err := os.WriteFile(filepath.Join(dir, cand), []byte("squatter"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return f
	}}
	return examine.Options{Drivers: []detect.Driver{{
		Name: "mtfs", Probe: fstest.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := fstest.Open(r, size)
			if err != nil {
				return nil, err
			}
			return hookedFS{FileSystem: fsys, h: hook}, nil
		},
	}}}
}

// ioFailImage is an EWF-format image over data whose reads of [failFrom, failTo)
// fail with a plain I/O error (not a chunk error).
type ioFailImage struct {
	data             []byte
	failFrom, failTo int64
	err              error
}

func (m *ioFailImage) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	end := min(off+int64(len(p)), int64(len(m.data)))
	if off < m.failTo && end > m.failFrom { // the read overlaps the failing range
		n := 0
		if off < m.failFrom {
			n = copy(p, m.data[off:m.failFrom])
		}
		return n, m.err
	}
	n := copy(p, m.data[off:end])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (m *ioFailImage) Size() int64        { return int64(len(m.data)) }
func (m *ioFailImage) SectorSize() int    { return 512 }
func (*ioFailImage) Format() string       { return "ewf" }
func (*ioFailImage) Metadata() []image.KV { return nil }
func (*ioFailImage) Close() error         { return nil }

// A real I/O error (not the container's chunk-corrupt error) in the middle of a
// file keeps its old behaviour: it is not downgraded to an incomplete artifact
// and a warning, it ends the analysis. A chunk error with the same shape is
// skipped (TestExtractUnreadableChunkMakesFileIncompleteAndContinues).
func TestExtractIOErrorIsNotDowngradedToIncompleteFile(t *testing.T) {
	big := pattern(8*e01Chunk, 5)
	nodes := []fstest.Node{
		{Path: "/a-first.txt", Data: []byte("first")},
		{Path: "/big.bin", Data: big},
		{Path: "/z-last.txt", Data: []byte("last file")},
	}
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes}))
	base := bytes.Index(data, big[:64])
	boom := errors.New("input/output error")
	stub := &ioFailImage{data: data, failFrom: int64(base + len(big)/2), failTo: int64(base + len(big)/2 + e01Chunk), err: boom}
	image.RegisterEWF(func(files []*os.File) (image.Image, error) {
		for _, f := range files {
			_ = f.Close()
		}
		return stub, nil
	})
	t.Cleanup(func() { image.RegisterEWF(nil) })

	c := newCase(t)
	s := openSession(t, c, ewfArtifact(t, c).ID)
	sum, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if !errors.Is(err, boom) {
		t.Fatalf("Extract err = %v, want the I/O error to end the analysis", err)
	}
	if len(auditByAction(t, c, "analysis.error")) != 1 || len(auditByAction(t, c, "analysis.end")) != 0 {
		t.Error("the analysis must end with analysis.error")
	}
	for _, r := range sum.Artifacts {
		if r.Source.RemotePath == "/z-last.txt" {
			t.Errorf("the run went on after an I/O error: %+v", r)
		}
	}
}

// When every candidate name of the prefix runs sidecar is taken (16 attempts),
// the prefix runs cannot be recorded: the partial artifact then carries NO runs
// (never the full list, which would claim bytes it does not hold), its error
// says so and names the full-run artifact, and the analysis still goes on.
func TestExtractPrefixSidecarNamesExhaustedRecordsNoRuns(t *testing.T) {
	const frags = evidence.MaxInlineRuns + 2000
	size := frags * blk
	big := pattern(size, 3)
	nodes := []fstest.Node{
		{Path: "/a-first.txt", Data: []byte("first")},
		{Path: "/big.bin", Data: big, Fragments: frags},
		{Path: "/z-last.txt", Data: []byte("last file")},
	}
	base := bytes.Index(disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})), big[:64])
	if base < 0 {
		t.Fatal("cannot find the file data in the disk")
	}
	c := newCase(t)
	s, _ := e01ChunkSessionOpts(t, c, nodes, int64(base+(frags-200)*2*blk), squatPrefixSidecarNames(t, c, "big.bin", 16))
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})

	byPath := map[string]evidence.ManifestRecord{}
	for _, r := range sum.Artifacts {
		byPath[r.Source.RemotePath] = r
	}
	rec := byPath["/big.bin"]
	if !rec.Incomplete || rec.Size == 0 {
		t.Fatalf("big.bin: incomplete=%v size=%d error=%q", rec.Incomplete, rec.Size, rec.Error)
	}
	d := rec.Source.Derived
	if len(d.Runs) != 0 || d.RunsArtifact != "" {
		t.Fatalf("the artifact must carry no runs: %d inline, sidecar %q", len(d.Runs), d.RunsArtifact)
	}
	full := fullRunsArtifact(t, c, "")
	if !strings.Contains(rec.Error, "not recorded") || !strings.Contains(rec.Error, full) {
		t.Errorf("error %q must say the prefix runs are not recorded and name %s", rec.Error, full)
	}
	var warned bool
	for _, e := range auditByAction(t, c, "analysis.warning") {
		if r, _ := e.Details["reason"].(string); strings.Contains(r, "partial artifact kept") && strings.Contains(r, full) {
			warned = true
		}
	}
	if !warned {
		t.Error("no analysis.warning names the full runs artifact")
	}
	if string(readArtifact(t, c, byPath["/z-last.txt"])) != "last file" || len(auditByAction(t, c, "analysis.end")) != 1 {
		t.Error("the analysis must go on and end normally")
	}
}
