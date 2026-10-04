package examine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

const e01Chunk = 8 * 512 // 8 sectors per chunk

// e01ChunkSession builds a GPT disk around an MTFS image holding the nodes,
// wraps it in a 3-segment E01 set, flips one byte of the stored chunk holding
// corruptAt (a disk offset; negative = no corruption), imports the set and
// opens it.
func e01ChunkSession(t *testing.T, c *evidence.Case, nodes []fstest.Node, corruptAt int64) (*examine.Session, []byte) {
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
	return openSession(t, c, recs[0].ID), data
}

// A chunk that cannot be decoded in the middle of one file makes that file an
// incomplete artifact (the bytes before the chunk, never zeros), with the
// provenance runs of that prefix only; the run goes on with the next files.
func TestExtractUnreadableChunkMakesFileIncompleteAndContinues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frags int // > MaxInlineRuns forces a runs sidecar
	}{{"inline runs", 0}, {"sidecar runs", evidence.MaxInlineRuns + 2000}} {
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
				corruptAt = int64(base + (tc.frags-200)*stride)
			}
			c := newCase(t)
			s, data := e01ChunkSession(t, c, nodes, corruptAt)
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
			if tc.frags > 0 {
				// A prefix this long still needs a sidecar; it must hold the prefix runs only.
				if len(runs) != 0 || d.RunsArtifact == "" {
					t.Fatalf("runs %d, sidecar %q", len(runs), d.RunsArtifact)
				}
				runs = readSidecarRuns(t, c, d.RunsArtifact)
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
			verifyOK(t, c)
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
