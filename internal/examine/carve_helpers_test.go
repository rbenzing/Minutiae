package examine

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

// The carving scope tests live inside the package: buildCarveScope, carveScope and allocClassifier are
// unexported, and the case, the image and the filesystem hook are built here (prefix carve) so nothing
// collides with the external test package's helpers.

const (
	carveBS       = 4096
	carveLinux    = "0fc63daf-8483-4772-8e79-3d69d8477de4"
	carveFirstLBA = 40
	carveGapLBA   = 8
)

// carveHook wraps an MTFS filesystem to alter what a scope sees.
type carveHook struct {
	filesys.FileSystem
	blockSize  int      // Info().BlockSize when > 0
	warnOnUnal []string // warnings that appear once Unallocated() has been called
	warnings   []string
	dropFree   *filesys.Run  // removed from the free runs (image-relative, partition-relative here)
	extraRuns  []filesys.Run // appended to the free runs (hostile or empty ones)
}

func (h *carveHook) Underlying() filesys.FileSystem { return h.FileSystem }

func (h *carveHook) Info() filesys.Info {
	i := h.FileSystem.Info()
	if h.blockSize > 0 {
		i.BlockSize = h.blockSize
	}
	i.Warnings = append(slices.Clone(i.Warnings), h.warnings...)
	return i
}

func (h *carveHook) Unallocated() ([]filesys.Run, error) {
	h.warnings = append(h.warnings, h.warnOnUnal...)
	runs, err := h.FileSystem.Unallocated()
	if h.dropFree != nil {
		var out []filesys.Run
		d := *h.dropFree
		for _, r := range runs {
			lo, hi := r.Offset, r.Offset+r.Length
			if hi <= d.Offset || lo >= d.Offset+d.Length {
				out = append(out, r)
				continue
			}
			if lo < d.Offset {
				out = append(out, filesys.Run{Offset: lo, Length: d.Offset - lo})
			}
			if hi > d.Offset+d.Length {
				out = append(out, filesys.Run{Offset: d.Offset + d.Length, Length: hi - d.Offset - d.Length})
			}
		}
		runs = out
	}
	runs = append(runs, h.extraRuns...)
	return runs, err
}

func carveNewCase(t *testing.T) *evidence.Case {
	t.Helper()
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "C", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// carveDisk lays the contents out as consecutive GPT partitions (numbered from 1).
func carveDisk(contents ...[]byte) []byte {
	type placed struct {
		lba uint64
		b   []byte
	}
	var parts []volumetest.Part
	var ps []placed
	lba := uint64(carveFirstLBA)
	for i, b := range contents {
		sectors := uint64(len(b)+511) / 512
		parts = append(parts, volumetest.Part{
			StartLBA: lba, Sectors: sectors, TypeGUID: carveLinux,
			GUID: "00000000-0000-0000-0000-00000000000" + string(rune('1'+i)), Name: "p",
		})
		ps = append(ps, placed{lba, b})
		lba += sectors + carveGapLBA
	}
	img := volumetest.GPT(512, lba+40, "11111111-2222-3333-4444-555555555555", parts)
	for _, p := range ps {
		copy(img[p.lba*512:], p.b)
	}
	return img
}

func carveFS(nodes ...fstest.Node) []byte {
	return fstest.Build(fstest.BuildSpec{Label: "L", BlockSize: carveBS, FreeBlocks: 4, Nodes: nodes})
}

// carveImport writes img to a temp file and imports it into c.
func carveImport(t *testing.T, c *evidence.Case, img []byte) evidence.ManifestRecord {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(p, img, 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := Import(context.Background(), c, "img", []string{p}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return recs[0]
}

// carveEnv is a case holding one imported image and a session over it whose filesystem reads go through hook.
type carveEnv struct {
	c    *evidence.Case
	s    *Session
	img  []byte
	rec  evidence.ManifestRecord
	hook *carveHook
}

func carveOpts(setup func(*carveHook)) (Options, *carveHook) {
	hook := &carveHook{}
	return Options{Drivers: []detect.Driver{{
		Name: "mtfs", Probe: fstest.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := fstest.Open(r, size)
			if err != nil {
				return nil, err
			}
			hook.FileSystem = fsys
			if setup != nil {
				setup(hook)
			}
			return hook, nil
		},
	}}}, hook
}

// newCarveImageEnv imports img and opens a session whose filesystems pass through a hook.
func newCarveImageEnv(t *testing.T, img []byte, setup func(*carveHook)) *carveEnv {
	t.Helper()
	c := carveNewCase(t)
	rec := carveImport(t, c, img)
	opts, hook := carveOpts(setup)
	s, err := Open(c, rec.ID, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &carveEnv{c: c, s: s, img: img, rec: rec, hook: hook}
}

// newCarveEnv builds an MTFS image of nodes inside a one-partition GPT disk.
func newCarveEnv(t *testing.T, nodes ...fstest.Node) *carveEnv {
	t.Helper()
	return newCarveImageEnv(t, carveDisk(carveFS(nodes...)), nil)
}

// carveLayout returns the filesystem-relative runs of the regular node at path in a plain build of nodes.
func carveLayout(t *testing.T, path string, nodes ...fstest.Node) []filesys.Run {
	t.Helper()
	plain := slices.Clone(nodes)
	for i := range plain {
		plain[i].Deleted, plain[i].Freed, plain[i].Recover = false, false, nil
	}
	img := carveFS(plain...)
	fsys, err := fstest.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	e, err := fsys.Lookup(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := fsys.Open(e)
	if err != nil {
		t.Fatal(err)
	}
	return f.Runs()
}

func carveFill(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%200) + 1
	}
	return b
}
