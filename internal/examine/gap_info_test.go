package examine_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// e01GapSession builds a GPT disk around an MTFS image holding nodes, declares
// gapSectors sectors at byte offset gapAt of the disk in an error2 section (a
// negative gapSectors declares everything from gapAt to the end of the disk),
// zeroes them when zero is set (what an acquirer does for sectors it could not
// read), wraps the disk in a 3-segment E01 set, imports it and opens it.
func e01GapSession(t *testing.T, c *evidence.Case, nodes []fstest.Node, gapAt, gapSectors int64, zero bool) (*examine.Session, []byte) {
	t.Helper()
	return e01GapSessionMut(t, c, nodes, gapAt, gapSectors, zero, nil)
}

// e01GapSessionMut is e01GapSession with mutate applied to the built segments.
func e01GapSessionMut(t *testing.T, c *evidence.Case, nodes []fstest.Node, gapAt, gapSectors int64, zero bool, mutate func([][]byte)) (*examine.Session, []byte) {
	t.Helper()
	return e01GapSessionOpts(t, c, nodes, gapAt, gapSectors, zero, mutate, mtfsOpts())
}

// e01GapSessionOpts is e01GapSessionMut with the session options given.
func e01GapSessionOpts(t *testing.T, c *evidence.Case, nodes []fstest.Node, gapAt, gapSectors int64, zero bool, mutate func([][]byte), opts examine.Options) (*examine.Session, []byte) {
	t.Helper()
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes}))
	if gapSectors < 0 {
		gapSectors = (int64(len(data)) - gapAt) / 512
	}
	if zero {
		clear(data[gapAt : gapAt+gapSectors*512])
	}
	o := ewftest.Options{
		SectorsPerChunk: 8, ChunksPerSegment: (len(data)/e01Chunk + 2) / 3, Compress: ewftest.CompressNone,
		ErrorRanges: [][2]uint32{{uint32(gapAt / 512), uint32(gapSectors)}},
	}
	segs := ewftest.Build(o, data)
	if mutate != nil {
		mutate(segs)
	}
	dir := t.TempDir()
	paths := make([]string, len(segs))
	for i, s := range segs {
		paths[i] = filepath.Join(dir, "disk.E0"+string(rune('1'+i)))
		if err := os.WriteFile(paths[i], s, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recs := importPaths(t, c, paths)
	s, err := examine.Open(c, recs[0].ID, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, data
}

func importPaths(t *testing.T, c *evidence.Case, paths []string) []evidence.ManifestRecord {
	t.Helper()
	recs, err := examine.Import(context.Background(), c, "img", paths, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return recs
}

func TestInfoReportsAcquisitionGaps(t *testing.T) {
	c := newCase(t)
	big := pattern(6*e01Chunk+100, 3)
	nodes := []fstest.Node{{Path: "/big.bin", Data: big}}
	base := bytes.Index(disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})), big[:64])
	s, _ := e01GapSession(t, c, nodes, int64(base)+512, 3, true)
	g := s.Info().AcquisitionGaps
	if g == nil || g.Ranges != 1 || len(g.Kept) != 1 || g.Kept[0].Offset != int64(base)+512 || g.Kept[0].Length != 3*512 {
		t.Fatalf("Info().AcquisitionGaps = %+v", g)
	}
}

func TestInfoRawImageHasNoAcquisitionGaps(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0, fstest.Node{Path: "/a.txt", Data: []byte("a")})
	if g := s.Info().AcquisitionGaps; g != nil {
		t.Fatalf("raw image reports acquisition gaps: %+v", g)
	}
}
