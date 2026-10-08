package examine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

const (
	partStart = int64(firstLBA * 512) // the first partition of disk()
	bs        = 4096                  // MTFS block size of the test images (the table then fits one block, so the layout does not depend on the maps)
	mtfsRecM  = "fat-contiguous"      // a deleted-file method the build has a rule for (base 55)
)

// buildBS is the block size the test images are built with (a test with a huge table lowers it).
var buildBS int64 = bs

// pat is n bytes that are never uniform (for n > 1) and never the value 0 or 255.
func pat(seed byte, n int) []byte {
	rng := rand.New(rand.NewSource(int64(seed))) //nolint:gosec // seeded test data, not security
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Intn(250) + 1)
	}
	return b
}

// plain strips deletion and recovery planting, giving the same layout as the real spec.
func plain(nodes []fstest.Node) []fstest.Node {
	out := slices.Clone(nodes)
	for i := range out {
		out[i].Deleted, out[i].Freed, out[i].Recover = false, false, nil
	}
	return out
}

// layout returns the filesystem-relative runs of every regular node, as the reader lays them out.
func layout(t *testing.T, nodes []fstest.Node) map[string][]filesys.Run {
	t.Helper()
	img := fstest.Build(fstest.BuildSpec{Label: "L", BlockSize: int(buildBS), FreeBlocks: 4, Nodes: plain(nodes)})
	fsys, err := fstest.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]filesys.Run{}
	for _, n := range nodes {
		if n.Dir || n.Link != "" || len(n.Data) == 0 {
			continue
		}
		e, err := fsys.Lookup(n.Path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := fsys.Open(e)
		if err != nil {
			t.Fatal(err)
		}
		out[n.Path] = f.Runs()
	}
	return out
}

// blocks returns blocks [i, i+n) of a contiguous single run.
func blocks(r []filesys.Run, i, n int) filesys.Run {
	return filesys.Run{Offset: r[0].Offset + int64(i)*bs, Length: int64(n) * bs}
}

type recEnv struct {
	c      *evidence.Case
	s      *examine.Session
	img    []byte
	layout map[string][]filesys.Run
}

// newRecEnv builds an MTFS image of nodes inside a GPT disk, imports it and opens a session.
func newRecEnv(t *testing.T, nodes ...fstest.Node) *recEnv {
	t.Helper()
	c := newCase(t)
	fsimg := fstest.Build(fstest.BuildSpec{Label: "L", BlockSize: int(buildBS), FreeBlocks: 4, Nodes: nodes})
	img := disk(fsimg)
	recs := importImage(t, c, img, 1)
	return &recEnv{c: c, s: openSession(t, c, recs[0].ID), img: img, layout: layout(t, nodes)}
}

// fsHook wraps an MTFS filesystem to observe or alter what the recovery run sees.
type fsHook struct {
	filesys.FileSystem
	onReadDir   func(dir filesys.Entry, kids []filesys.Entry)
	failReadDir func(dir filesys.Entry) error
	rewriteKids func(kids []filesys.Entry) []filesys.Entry
	recoverable func(inner filesys.Recoverer, e filesys.Entry) ([]filesys.Candidate, error)
	unalloc     func(inner []filesys.Run) []filesys.Run
	warnOnUnal  []string
	warnings    []string
}

func (h *fsHook) Underlying() filesys.FileSystem { return h.FileSystem }

func (h *fsHook) Info() filesys.Info {
	i := h.FileSystem.Info()
	i.Warnings = append(slices.Clone(i.Warnings), h.warnings...)
	return i
}

func (h *fsHook) ReadDir(dir filesys.Entry) ([]filesys.Entry, error) {
	if h.failReadDir != nil {
		if err := h.failReadDir(dir); err != nil {
			return nil, err
		}
	}
	kids, err := h.FileSystem.ReadDir(dir)
	if h.rewriteKids != nil && err == nil {
		kids = h.rewriteKids(slices.Clone(kids))
	}
	if h.onReadDir != nil && err == nil {
		h.onReadDir(dir, kids)
	}
	return kids, err
}

func (h *fsHook) Unallocated() ([]filesys.Run, error) {
	h.warnings = append(h.warnings, h.warnOnUnal...)
	runs, err := h.FileSystem.Unallocated()
	if h.unalloc != nil {
		runs = h.unalloc(runs)
	}
	return runs, err
}

func (h *fsHook) Recoverable(e filesys.Entry) ([]filesys.Candidate, error) {
	inner, ok := filesys.As[filesys.Recoverer](h.FileSystem)
	if !ok {
		return nil, filesys.ErrNoRecovery
	}
	if h.recoverable != nil {
		return h.recoverable(inner, e)
	}
	return inner.Recoverable(e)
}

// newHookEnv is newRecEnv with every opened filesystem wrapped in an fsHook (returned).
func newHookEnv(t *testing.T, setup func(*fsHook), nodes ...fstest.Node) (*recEnv, *fsHook) {
	t.Helper()
	c := newCase(t)
	fsimg := fstest.Build(fstest.BuildSpec{Label: "L", BlockSize: int(buildBS), FreeBlocks: 4, Nodes: nodes})
	img := disk(fsimg)
	recs := importImage(t, c, img, 1)
	hook := &fsHook{}
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: []detect.Driver{{
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
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &recEnv{c: c, s: s, img: img, layout: layout(t, nodes)}, hook
}

func (e *recEnv) recover(t *testing.T, o examine.RecoverOptions) examine.RecoverSummary {
	t.Helper()
	if o.Partition == 0 {
		o.Partition = -1
	}
	sum, err := e.s.Recover(context.Background(), o)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return sum
}

// recovered returns the manifest records of kind recover, by path.
func recovered(t *testing.T, c *evidence.Case) []evidence.ManifestRecord {
	t.Helper()
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.ManifestRecord
	for _, r := range recs {
		if r.Source.Kind == evidence.KindRecover {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b evidence.ManifestRecord) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// warnReasons returns the reason of every analysis.warning that is a skip (no filesystem source).
func warnReasons(t *testing.T, c *evidence.Case) []string {
	t.Helper()
	var out []string
	for _, e := range auditByAction(t, c, "analysis.warning") {
		if e.Details["source"] != nil {
			continue
		}
		r, _ := e.Details["reason"].(string)
		out = append(out, r)
	}
	return out
}

func anyContains(list []string, sub string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.Contains(s, sub) })
}

// delNode is a deleted, freed node with the given maps (none = the map of its own blocks).
func delNode(path string, data []byte, maps ...fstest.RecoverMap) fstest.Node {
	if len(maps) == 0 {
		maps = []fstest.RecoverMap{{Method: mtfsRecM}}
	}
	return fstest.Node{Path: path, Data: data, Deleted: true, Freed: true, Recover: maps}
}

func TestRecoverNeverCapturesAllocatedBytes(t *testing.T) {
	a, b, c := pat(1, 4*bs), pat(2, 2*bs), pat(3, 2*bs)
	nodes := []fstest.Node{
		delNode("/a-del.bin", a),
		{Path: "/b-live.bin", Data: b},
		delNode("/c-del.bin", c),
	}
	lay := layout(t, nodes)
	// a's map starts free and runs into the live file; c's map lies entirely over live data.
	nodes[0].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 6 * bs, Runs: []filesys.Run{lay["/a-del.bin"][0], lay["/b-live.bin"][0]}}}
	nodes[2].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 2 * bs, Runs: []filesys.Run{lay["/b-live.bin"][0]}}}
	e := newRecEnv(t, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true})

	recs := recovered(t, e.c)
	if len(recs) != 1 {
		t.Fatalf("%d recovered artifacts, want exactly the free prefix of a-del.bin (summary %+v)", len(recs), sum)
	}
	rec := recs[0]
	art := readArtifact(t, e.c, rec)
	if !bytes.Equal(art, a) {
		t.Errorf("artifact is %d bytes, want exactly the 4 free blocks of a-del.bin", len(art))
	}
	if bytes.Contains(art, b[:32]) {
		t.Error("a byte pattern of the live file reached the artifact")
	}
	d := rec.Source.Derived
	wantRuns := []evidence.Run{{Offset: partStart + lay["/a-del.bin"][0].Offset, Length: 4 * bs}}
	if !slices.Equal(d.Runs, wantRuns) {
		t.Errorf("runs = %v, want only the captured prefix %v", d.Runs, wantRuns)
	}
	r := d.Recovery
	if r.Alloc.ExcludedBytes != 2*bs || r.Alloc.ExcludedRuns != 1 || len(r.Excluded) != 1 || r.Excluded[0].Length != 2*bs {
		t.Errorf("excluded = %v alloc = %+v, want the live run named and counted", r.Excluded, r.Alloc)
	}
	if r.Alloc.Free != 4*bs || r.Alloc.Allocated != 0 || r.Alloc.Unknown != 0 {
		t.Errorf("alloc = %+v, want 2048 free bytes captured", r.Alloc)
	}
	if !rec.Incomplete || rec.Error == "" {
		t.Errorf("incomplete=%v error=%q, want a flagged partial artifact with its reason", rec.Incomplete, rec.Error)
	}
	if sum.SkippedBy["no-free-bytes"] != 1 || sum.Recovered != 1 || sum.Partial != 1 {
		t.Errorf("summary = %+v, want one no-free-bytes skip, 1 recovered, 1 partial", sum)
	}
	verifyOK(t, e.c)
}

func TestRecoverReusedBlocksKeepFreePrefix(t *testing.T) {
	a, b := pat(1, 4*bs), pat(2, 2*bs)
	nodes := []fstest.Node{delNode("/a-del.bin", a), {Path: "/b-live.bin", Data: b}}
	lay := layout(t, nodes)
	ar := lay["/a-del.bin"]
	nodes[0].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 3 * bs, Runs: []filesys.Run{
		blocks(ar, 0, 1), blocks(lay["/b-live.bin"], 0, 1), blocks(ar, 3, 1),
	}}}
	e := newRecEnv(t, nodes...)
	e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != 1 {
		t.Fatalf("%d artifacts, want 1", len(recs))
	}
	if art := readArtifact(t, e.c, recs[0]); !bytes.Equal(art, a[:bs]) {
		t.Errorf("artifact = %d bytes, want only block 0: a free run after a reused block must not be captured", len(art))
	}
	r := recs[0].Source.Derived.Recovery
	if r.Alloc.ExcludedRuns != 2 || r.Alloc.ExcludedBytes != 2*bs {
		t.Errorf("alloc = %+v, want the reused block and the later free block excluded", r.Alloc)
	}
	if !slices.Contains(r.Assumptions, "prefix-only") || !slices.Contains(r.Assumptions, "cut-state=allocated") {
		t.Errorf("assumptions = %q", r.Assumptions)
	}
}

func TestRecoverNeverZeroFills(t *testing.T) {
	a := pat(1, 4*bs)
	nodes := []fstest.Node{delNode("/a-del.bin", a), delNode("/b-del.bin", pat(2, 4*bs))}
	lay := layout(t, nodes)
	// a's map: a free block, then a gap into the metadata region (not free); b's map is short (one block of 2).
	nodes[0].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 2 * bs, Runs: []filesys.Run{
		blocks(lay["/a-del.bin"], 0, 1), {Offset: 0, Length: bs},
	}}}
	nodes[1].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 2 * bs, Runs: []filesys.Run{blocks(lay["/b-del.bin"], 0, 1)}}}
	e := newRecEnv(t, nodes...)
	e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != 2 {
		t.Fatalf("%d artifacts, want 2", len(recs))
	}
	for _, rec := range recs {
		art := readArtifact(t, e.c, rec)
		if len(art) != bs {
			t.Errorf("%s holds %d bytes, want 512 (no padding for the part the map could not supply)", rec.Path, len(art))
		}
		if !rec.Incomplete {
			t.Errorf("%s not flagged incomplete", rec.Path)
		}
		if !bytes.Equal(art, readImageRuns(e.img, rec.Source.Derived.Runs)) {
			t.Errorf("%s differs from the image at its runs", rec.Path)
		}
	}
	if rs := recs[1].Source.Derived.Recovery.Assumptions; !slices.Contains(rs, fmt.Sprintf("size-declared=%d", 2*bs)) {
		t.Errorf("b-del assumptions = %q, want size-declared=2*bs", rs)
	}
}

func TestRecoveredArtifactEqualsRuns(t *testing.T) {
	data := pat(5, 6*bs)
	frag := fstest.Node{Path: "/frag.bin", Data: data, Deleted: true, Freed: true, Fragments: 3, Recover: []fstest.RecoverMap{{Method: mtfsRecM}}}
	e := newRecEnv(t, frag, delNode("/whole.bin", pat(6, 3*bs)))
	e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != 2 {
		t.Fatalf("%d artifacts", len(recs))
	}
	for _, rec := range recs {
		art := readArtifact(t, e.c, rec)
		if got := readImageRuns(e.img, rec.Source.Derived.Runs); !bytes.Equal(got, art) {
			t.Errorf("%s: image bytes at the runs differ from the artifact", rec.Path)
		}
		if int64(len(art)) != rec.Size {
			t.Errorf("%s: size %d, bytes %d", rec.Path, rec.Size, len(art))
		}
	}
	if !bytes.Equal(readArtifact(t, e.c, recs[0]), data) {
		t.Error("fragmented file not reproduced in file order")
	}
	if len(recs[0].Source.Derived.Runs) != 3 {
		t.Errorf("runs = %v, want 3 fragments", recs[0].Source.Derived.Runs)
	}
}

func TestRecoverMapPointingAtMetadataIsCut(t *testing.T) {
	nodes := []fstest.Node{delNode("/a-del.bin", pat(1, 2*bs))}
	nodes[0].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 2 * bs, Runs: []filesys.Run{{Offset: 0, Length: 2 * bs}}}}
	e := newRecEnv(t, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if n := len(recovered(t, e.c)); n != 0 {
		t.Fatalf("%d artifacts from a map over the header and table region", n)
	}
	if sum.SkippedBy["no-free-bytes"] != 1 || !anyContains(warnReasons(t, e.c), "no-free-bytes") {
		t.Errorf("summary %+v, warnings %q", sum, warnReasons(t, e.c))
	}
}

func TestRecoverUnknownStateWhenUnallocatedWarns(t *testing.T) {
	a, b := pat(1, 2*bs), pat(2, 2*bs)
	nodes := []fstest.Node{delNode("/a-del.bin", a), {Path: "/b-live.bin", Data: b}}
	lay := layout(t, nodes)
	nodes[0].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 3 * bs, Runs: []filesys.Run{
		blocks(lay["/a-del.bin"], 0, 2), blocks(lay["/b-live.bin"], 0, 1),
	}}}
	e, _ := newHookEnv(t, func(h *fsHook) { h.warnOnUnal = []string{"bitmap group 3 skipped"} }, nodes...)
	e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != 1 {
		t.Fatalf("%d artifacts", len(recs))
	}
	if art := readArtifact(t, e.c, recs[0]); !bytes.Equal(art, a) {
		t.Errorf("artifact is %d bytes, want only the two bytes-proven-free blocks", len(art))
	}
	if as := recs[0].Source.Derived.Recovery.Assumptions; !slices.Contains(as, "cut-state=unknown") {
		t.Errorf("assumptions = %q, want cut-state=unknown", as)
	}
}

func TestRecoverRecordsFullProvenance(t *testing.T) {
	data := pat(9, 3*bs)
	n := delNode("/dir/gone.txt", data, fstest.RecoverMap{
		Method: mtfsRecM, Basis: []string{"stale inode"}, Assumptions: []string{"not reused"}, Mode: 0o640,
	})
	n.MTime = 1700000000
	e := newRecEnv(t, fstest.Node{Path: "/dir", Dir: true}, n)
	sum := e.recover(t, examine.RecoverOptions{All: true, MinConfidence: 10})
	recs := recovered(t, e.c)
	if len(recs) != 1 {
		t.Fatalf("%d artifacts (%+v)", len(recs), sum)
	}
	rec := recs[0]
	if rec.Source.Kind != "recover" || rec.Source.RemotePath != "" {
		t.Errorf("kind %q remote path %q", rec.Source.Kind, rec.Source.RemotePath)
	}
	if !strings.Contains(rec.Path, "/recovered/p1-mtfs/000001-gone.txt") {
		t.Errorf("path = %q", rec.Path)
	}
	d := rec.Source.Derived
	parent := e.s.Parent
	if d.ParentID != parent.ID || d.ParentSHA256 != parent.SHA256 || d.Partition != 1 || d.PartitionOffset != partStart || d.FSType != "mtfs" {
		t.Errorf("derivation = %+v", d)
	}
	if d.FSPath != "/dir/gone.txt" || d.FSID == "" || d.Mode != 0o640 || d.Times["modified"] == "" {
		t.Errorf("fs path/id/mode/times = %q %q %o %v", d.FSPath, d.FSID, d.Mode, d.Times)
	}
	lay := e.layout["/dir/gone.txt"]
	if want := []evidence.Run{{Offset: partStart + lay[0].Offset, Length: 3 * bs}}; !slices.Equal(d.Runs, want) {
		t.Errorf("runs = %v, want image-relative %v", d.Runs, want)
	}
	r := d.Recovery
	if r == nil {
		t.Fatal("no Recovery")
	}
	if r.Class != evidence.ClassDeletedFile || r.Method != mtfsRecM || r.Confidence == nil || *r.Confidence != 55 {
		t.Errorf("class/method/confidence = %q %q %v", r.Class, r.Method, r.Confidence)
	}
	if r.Algorithm != evidence.AlgorithmRecover || r.Content != "ok" {
		t.Errorf("algorithm %q content %q", r.Algorithm, r.Content)
	}
	if !slices.Contains(r.Basis, "stale inode") || !slices.Contains(r.Assumptions, "not reused") {
		t.Errorf("basis %q assumptions %q", r.Basis, r.Assumptions)
	}
	if r.Alloc.Free != 3*bs || r.Alloc.ExcludedRuns != 0 {
		t.Errorf("alloc = %+v", r.Alloc)
	}
	wantParams := map[string]string{"min_confidence": "10", "all_candidates": "false", "keep_uniform": "false"}
	for k, v := range wantParams {
		if r.Params[k] != v {
			t.Errorf("param %s = %q, want %q", k, r.Params[k], v)
		}
	}
	if probs := r.Check(evidence.KindRecover); len(probs) != 0 {
		t.Errorf("Recovery.Check: %v", probs)
	}
	if rec.Incomplete {
		t.Error("a whole recovery must not be flagged incomplete")
	}
	verifyOK(t, e.c)
}

func TestRecoverFromSplitImageRecordsAllParentSegments(t *testing.T) {
	c := newCase(t)
	nodes := []fstest.Node{delNode("/a.bin", pat(1, 3*bs))}
	img := disk(fstest.Build(fstest.BuildSpec{Label: "L", BlockSize: int(buildBS), FreeBlocks: 4, Nodes: nodes}))
	recs := importImage(t, c, img, 3)
	s := openSession(t, c, recs[0].ID)
	if _, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true}); err != nil {
		t.Fatal(err)
	}
	out := recovered(t, c)
	if len(out) != 1 {
		t.Fatalf("%d artifacts", len(out))
	}
	if got := out[0].Source.Derived.ParentSegments; len(got) != 3 {
		t.Errorf("parent segments = %v, want 3", got)
	}
	verifyOK(t, c)
}

func TestRecoverIncompleteParentIsFlagged(t *testing.T) {
	c := newCase(t)
	nodes := []fstest.Node{delNode("/a.bin", pat(1, 3*bs))}
	img := disk(fstest.Build(fstest.BuildSpec{Label: "L", BlockSize: int(buildBS), FreeBlocks: 4, Nodes: nodes}))
	recs := importImage(t, c, img, 1)
	s := openSession(t, c, recs[0].ID)
	s.Segments[0].Incomplete = true
	if _, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true}); err != nil {
		t.Fatal(err)
	}
	out := recovered(t, c)
	if len(out) != 1 || !out[0].Source.Derived.ParentIncomplete {
		t.Fatalf("artifacts %d, parent_incomplete not set", len(out))
	}
}

func TestRecoverEncryptedFlagged(t *testing.T) {
	listing := delNode("/listing.bin", pat(1, bs))
	listing.Encrypted = true
	reader := delNode("/reader.bin", pat(2, bs), fstest.RecoverMap{Method: mtfsRecM, Encrypted: true})
	plainN := delNode("/plain.bin", pat(3, bs))
	e := newRecEnv(t, listing, reader, plainN)
	e.recover(t, examine.RecoverOptions{All: true})
	got := map[string]bool{}
	for _, r := range recovered(t, e.c) {
		got[r.Source.Derived.FSPath] = r.Source.Derived.Encrypted
	}
	want := map[string]bool{"/listing.bin": true, "/reader.bin": true, "/plain.bin": false}
	for p, w := range want {
		if g, ok := got[p]; !ok || g != w {
			t.Errorf("%s: encrypted = %v (present %v), want %v", p, g, ok, w)
		}
	}
}

func TestRecoverRunsSidecarOver4096Runs(t *testing.T) {
	const n = evidence.MaxInlineRuns + 4
	const sbs = 512 // small blocks keep the 4100-fragment image small
	buildBS = sbs
	t.Cleanup(func() { buildBS = bs })
	data := pat(1, n*sbs)
	node := fstest.Node{Path: "/many.bin", Data: data, Deleted: true, Freed: true, Fragments: n, Recover: []fstest.RecoverMap{{Method: mtfsRecM}}}
	e := newRecEnv(t, node)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if len(sum.Artifacts) != 2 || sum.Recovered != 1 {
		t.Fatalf("artifacts %d recovered %d, want the file and its sidecar", len(sum.Artifacts), sum.Recovered)
	}
	var file, side evidence.ManifestRecord
	for _, r := range sum.Artifacts {
		if r.Source.Kind == "runs" {
			side = r
		} else {
			file = r
		}
	}
	if file.Source.Derived.Recovery == nil || side.Source.Derived.Recovery != nil {
		t.Errorf("recovery on file=%v on sidecar=%v, want only on the file", file.Source.Derived.Recovery != nil, side.Source.Derived.Recovery != nil)
	}
	if side.Source.RemotePath != "" {
		t.Errorf("sidecar remote path = %q, want empty", side.Source.RemotePath)
	}
	if filepath.Dir(side.Path) != filepath.Dir(file.Path) {
		t.Errorf("sidecar %q not beside %q", side.Path, file.Path)
	}
	runs, err := e.c.DerivedRuns(file, nil)
	if err != nil || len(runs) != n {
		t.Fatalf("DerivedRuns = %d runs, %v", len(runs), err)
	}
	if !bytes.Equal(readImageRuns(e.img, runs), data) {
		t.Error("runs do not reproduce the content")
	}
	verifyOK(t, e.c)
}

func TestRecoverSkipsUniformWithWarning(t *testing.T) {
	zero, ff := make([]byte, 2*bs), bytes.Repeat([]byte{0xFF}, 2*bs)
	e := newRecEnv(t, delNode("/z.bin", zero), delNode("/f.bin", ff), delNode("/ok.bin", pat(1, 2*bs)))
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if n := len(recovered(t, e.c)); n != 1 {
		t.Fatalf("%d artifacts, want only ok.bin", n)
	}
	if sum.SkippedBy["uniform"] != 2 || sum.Uniform != 2 {
		t.Errorf("summary = %+v", sum)
	}
	if got := warnReasons(t, e.c); strings.Count(strings.Join(got, "|"), "uniform") != 2 {
		t.Errorf("warnings = %q, want two uniform warnings", got)
	}
}

func TestRecoverKeepUniformWritesIt(t *testing.T) {
	e := newRecEnv(t, delNode("/z.bin", make([]byte, 2*bs)))
	sum := e.recover(t, examine.RecoverOptions{All: true, KeepUniform: true})
	recs := recovered(t, e.c)
	if len(recs) != 1 || recs[0].Source.Derived.Recovery.Content != "uniform" {
		t.Fatalf("artifacts %d, summary %+v", len(recs), sum)
	}
	if recs[0].Source.Derived.Recovery.Params["keep_uniform"] != "true" {
		t.Errorf("params = %v", recs[0].Source.Derived.Recovery.Params)
	}
}

func TestRecoverNonUniformContentIsOk(t *testing.T) {
	e := newRecEnv(t, delNode("/ok.bin", pat(1, 2*bs)))
	e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != 1 || recs[0].Source.Derived.Recovery.Content != "ok" {
		t.Fatalf("artifacts %d", len(recs))
	}
}

func TestRecoverAuditStartPrecedesFirstArtifact(t *testing.T) {
	zero := make([]byte, bs)
	nodes := []fstest.Node{delNode("/a.bin", pat(1, bs)), delNode("/z.bin", zero), delNode("/b.bin", pat(2, bs))}
	nodes[1].Recover = []fstest.RecoverMap{{Method: mtfsRecM}}
	e := newRecEnv(t, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	var start, warn, firstArt, end int64
	var endDetails map[string]any
	for _, en := range auditEntries(t, e.c) {
		switch en.Action {
		case "analysis.start":
			start = en.Seq
		case "analysis.warning":
			if warn == 0 {
				warn = en.Seq
			}
		case "artifact.create":
			if firstArt == 0 && start > 0 {
				firstArt = en.Seq
			}
		case "analysis.end":
			end, endDetails = en.Seq, en.Details
		}
	}
	if start <= 0 || start >= warn || warn >= firstArt || firstArt >= end {
		t.Errorf("order start=%d warning=%d artifact=%d end=%d", start, warn, firstArt, end)
	}
	same := func(k string, want any) bool { return fmt.Sprint(endDetails[k]) == fmt.Sprint(want) }
	if !same("files", sum.Files) || !same("bytes", sum.Bytes) || !same("skipped", sum.Skipped) {
		t.Errorf("analysis.end = %v, summary %+v", endDetails, sum)
	}
	if !same("recovered", sum.Recovered) || !same("considered", sum.Considered) {
		t.Errorf("analysis.end counters = %v", endDetails)
	}
	sb, _ := endDetails["skipped_by"].(map[string]any)
	if fmt.Sprint(sb["uniform"]) != "1" {
		t.Errorf("skipped_by = %v", endDetails["skipped_by"])
	}
}

func TestRecoverListMatchesRun(t *testing.T) {
	nodes := []fstest.Node{
		delNode("/a.bin", pat(1, 2*bs)), delNode("/z.bin", make([]byte, bs)),
		delNode("/m.bin", pat(3, 3*bs), fstest.RecoverMap{Method: mtfsRecM}, fstest.RecoverMap{Method: "ext-inode-intact"}),
	}
	e := newRecEnv(t, nodes...)
	// extract first: a session that already used the filesystem must plan the same (C5).
	if _, err := e.s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true}); err != nil {
		t.Fatal(err)
	}
	before := len(auditEntries(t, e.c))
	files := len(mustManifest(t, e.c))
	plan, err := e.s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true, AllCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(auditEntries(t, e.c)); got != before {
		t.Errorf("PlanRecovery appended %d audit entries", got-before)
	}
	if got := len(mustManifest(t, e.c)); got != files {
		t.Errorf("PlanRecovery wrote %d files", got-files)
	}
	type key struct {
		method string
		conf   int
		runs   string
		size   int64
	}
	var planned []key
	for _, it := range plan.Items {
		for _, c := range it.Candidates {
			if c.Skip == "" {
				planned = append(planned, key{c.Method, c.Confidence, fmt.Sprint(c.Runs), c.Size})
			}
		}
	}
	e.recover(t, examine.RecoverOptions{All: true, AllCandidates: true})
	var wrote []key
	for _, r := range recovered(t, e.c) {
		d := r.Source.Derived
		wrote = append(wrote, key{d.Recovery.Method, *d.Recovery.Confidence, fmt.Sprint(d.Runs), r.Size})
	}
	slices.SortFunc(planned, func(a, b key) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
	slices.SortFunc(wrote, func(a, b key) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
	if len(planned) == 0 || !slices.Equal(planned, wrote) {
		t.Errorf("planned %v\nwrote   %v", planned, wrote)
	}
	if plan.SkippedBy["uniform"] != 1 {
		t.Errorf("plan skipped = %v", plan.SkippedBy)
	}
}

func mustManifest(t *testing.T, c *evidence.Case) []evidence.ManifestRecord {
	t.Helper()
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestRecoverFSWarningsForwarded(t *testing.T) {
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.warnings = []string{"opened with a note"}
		h.warnOnUnal = []string{"free map group skipped"}
	}, delNode("/a.bin", pat(1, bs)))
	sum := e.recover(t, examine.RecoverOptions{All: true})
	got := fsWarnings(t, e.c)
	if !slices.Contains(got["filesystem-open"], "opened with a note") {
		t.Errorf("filesystem-open = %q", got["filesystem-open"])
	}
	if !slices.Contains(got["filesystem"], "free map group skipped") {
		t.Errorf("filesystem = %q", got["filesystem"])
	}
	if sum.FSWarnings != 1 {
		t.Errorf("FSWarnings = %d, want the one raised during the run", sum.FSWarnings)
	}
}

func TestRecoverRefsSelectEntries(t *testing.T) {
	nodes := []fstest.Node{
		{Path: "/d", Dir: true},
		delNode("/d/in.bin", pat(1, bs)), delNode("/out.bin", pat(2, bs)),
		{Path: "/live.bin", Data: pat(3, bs)},
	}
	e := newRecEnv(t, nodes...)
	ctx := context.Background()
	// a live directory path selects only deleted entries below it
	sum, err := e.s.Recover(ctx, examine.RecoverOptions{Partition: -1, Refs: []string{"/d"}})
	if err != nil || len(recovered(t, e.c)) != 1 || sum.Considered != 1 {
		t.Fatalf("dir ref: err %v artifacts %d considered %d", err, len(recovered(t, e.c)), sum.Considered)
	}
	// an id of a deleted entry, named twice with the directory ref: one artifact each, no duplicates
	e2 := newRecEnv(t, nodes...)
	id := idOf2(t, e2, "/out.bin")
	dirIn := idOf2(t, e2, "/d/in.bin")
	sum2, err := e2.s.Recover(ctx, examine.RecoverOptions{Partition: -1, Refs: []string{"id:" + id, "id:" + id, "/d", "id:" + dirIn}})
	if err != nil || len(recovered(t, e2.c)) != 2 || sum2.Considered != 2 {
		t.Fatalf("id refs: err %v artifacts %d considered %d", err, len(recovered(t, e2.c)), sum2.Considered)
	}
	// a live file path, a live id and an unknown id are errors
	e3 := newRecEnv(t, nodes...)
	if _, err := e3.s.Recover(ctx, examine.RecoverOptions{Partition: -1, Refs: []string{"/live.bin"}}); !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("live file path: %v, want ErrNotDeleted", err)
	}
	if _, err := e3.s.Recover(ctx, examine.RecoverOptions{Partition: -1, Refs: []string{"id:" + idOf2(t, e3, "/live.bin")}}); !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("live id: %v, want ErrNotDeleted", err)
	}
	if _, err := e3.s.Recover(ctx, examine.RecoverOptions{Partition: -1, Refs: []string{"id:no-such-id"}}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("unknown id: %v, want ErrNotFound", err)
	}
	if n := len(recovered(t, e3.c)); n != 0 {
		t.Errorf("%d artifacts from refused refs", n)
	}
}

// idOf2 finds the entry id of path p (live or deleted) by walking the filesystem.
func idOf2(t *testing.T, e *recEnv, p string) string {
	t.Helper()
	fsys, _, err := e.s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	_ = filesys.Walk(fsys, fsys.Root(), "/", func(wp string, we filesys.Entry, _ error) error {
		if wp == p {
			id = we.ID
		}
		return nil
	})
	if id == "" {
		t.Fatalf("no entry at %s", p)
	}
	return id
}

func TestRecoverDeletedDirectoryAndOtherTypesSkipped(t *testing.T) {
	nodes := []fstest.Node{
		{Path: "/gone", Dir: true, Deleted: true},
		{Path: "/link", Link: "/target", Deleted: true, Freed: true, Recover: []fstest.RecoverMap{{Method: mtfsRecM, Size: 7, Runs: []filesys.Run{{Offset: 512, Length: 7}}}}},
		delNode("/f.bin", pat(1, bs)),
	}
	e := newRecEnv(t, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if sum.SkippedBy["not-a-file"] != 1 {
		t.Errorf("skipped_by = %v, want the deleted directory counted not-a-file", sum.SkippedBy)
	}
	for _, r := range recovered(t, e.c) {
		if r.Source.Derived.FSPath == "/gone" {
			t.Error("a deleted directory was recovered")
		}
	}
}

func TestRecoverUnsupportedFilesystem(t *testing.T) {
	c := newCase(t)
	img := disk(fstest.Build(fstest.BuildSpec{Label: "L", BlockSize: int(buildBS), FreeBlocks: 4, Nodes: []fstest.Node{{Path: "/a", Data: []byte("x")}}}))
	recs := importImage(t, c, img, 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: []detect.Driver{{
		Name: "mtfs", Probe: fstest.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := fstest.Open(r, size)
			if err != nil {
				return nil, err
			}
			return struct{ filesys.FileSystem }{fsys}, nil // hides Recoverer: a driver without recovery
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	before := len(auditEntries(t, c))
	_, err = s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if !errors.Is(err, filesys.ErrNoRecovery) {
		t.Fatalf("err = %v, want ErrNoRecovery", err)
	}
	if got := len(auditEntries(t, c)); got != before || len(auditByAction(t, c, "analysis.start")) != 0 {
		t.Errorf("audit grew by %d, analysis.start written", got-before)
	}
	if _, err := s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true}); !errors.Is(err, filesys.ErrNoRecovery) {
		t.Errorf("PlanRecovery err = %v", err)
	}
}

func TestRecoverIOErrorFromRecovererAbortsRun(t *testing.T) {
	boom := errors.New("disk on fire")
	nodes := []fstest.Node{delNode("/a.bin", pat(1, bs)), delNode("/b.bin", pat(2, bs)), delNode("/c.bin", pat(3, bs))}
	run := func(t *testing.T, failWith error) (*recEnv, error) {
		e, _ := newHookEnv(t, func(h *fsHook) {
			h.recoverable = func(in filesys.Recoverer, en filesys.Entry) ([]filesys.Candidate, error) {
				if en.Name == "b.bin" {
					return nil, failWith
				}
				return in.Recoverable(en)
			}
		}, nodes...)
		_, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
		return e, err
	}
	e, err := run(t, boom)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the I/O error", err)
	}
	if len(auditByAction(t, e.c, "analysis.error")) != 1 || len(auditByAction(t, e.c, "analysis.end")) != 0 {
		t.Error("the run must end with analysis.error")
	}
	if n := len(recovered(t, e.c)); n != 0 {
		t.Errorf("%d artifacts, want none: the plan fails before any write", n)
	}
	e, err = run(t, &filesys.CorruptError{Structure: "x", Offset: -1, Reason: "bad"})
	if err != nil {
		t.Fatalf("a corrupt entry must only be skipped: %v", err)
	}
	if n := len(recovered(t, e.c)); n != 2 {
		t.Errorf("%d artifacts, want the 2 other entries", n)
	}
	if !anyContains(warnReasons(t, e.c), "corrupt") {
		t.Errorf("warnings = %q", warnReasons(t, e.c))
	}
}

func TestRecoverRecoversPanic(t *testing.T) {
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.recoverable = func(in filesys.Recoverer, en filesys.Entry) ([]filesys.Candidate, error) {
			if en.Name == "b.bin" {
				panic("hostile reader")
			}
			return in.Recoverable(en)
		}
	}, delNode("/a.bin", pat(1, bs)), delNode("/b.bin", pat(2, bs)), delNode("/c.bin", pat(3, bs)))
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if sum.Recovered != 2 || sum.SkippedBy["corrupt"] != 1 {
		t.Errorf("summary = %+v, want 2 recovered and the panic skipped as corrupt", sum)
	}
	if !anyContains(warnReasons(t, e.c), "corrupt") {
		t.Errorf("warnings = %q", warnReasons(t, e.c))
	}
}

func TestRecoverCancelledKeepsPartialFlagged(t *testing.T) {
	data := pat(1, 400*bs) // enough chunks for a cancel to land mid-copy
	e := newRecEnv(t, delNode("/big.bin", data))
	ctx, cancel := context.WithCancel(context.Background())
	var seen int64
	_, err := e.s.Recover(ctx, examine.RecoverOptions{Partition: -1, All: true, Progress: func(done, _ int64) {
		seen = done
		if done >= 64*bs {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a cancel", err)
	}
	recs := recovered(t, e.c)
	if len(recs) != 1 || !recs[0].Incomplete || recs[0].Error == "" {
		t.Fatalf("artifacts %+v (progress %d), want one partial flagged artifact", recs, seen)
	}
	art := readArtifact(t, e.c, recs[0])
	if !bytes.Equal(art, data[:len(art)]) || int64(len(art)) != recs[0].Size || len(art) == 0 || len(art) >= len(data) {
		t.Errorf("partial artifact is %d of %d bytes", len(art), len(data))
	}
	if got := readImageRuns(e.img, recs[0].Source.Derived.Runs); !bytes.Equal(got, art) {
		t.Error("the recorded runs must cover exactly the bytes kept")
	}
	if len(auditByAction(t, e.c, "analysis.error")) != 1 {
		t.Error("want analysis.error")
	}
}

func TestRecoverCaseWriteFailureIsNeverDowngradedToWarning(t *testing.T) {
	for name, failure := range map[string]error{"other": errors.New("create refused"), "enospc": syscall.ENOSPC} {
		t.Run(name, func(t *testing.T) {
			e := newRecEnv(t, delNode("/a.bin", pat(1, bs)), delNode("/b.bin", pat(2, bs)))
			e.s.SetNewArtifact(func(string, string, string, evidence.Source) (*evidence.ArtifactWriter, error) {
				return nil, failure
			})
			_, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
			if !errors.Is(err, failure) {
				t.Fatalf("err = %v, want the case failure", err)
			}
			if len(auditByAction(t, e.c, "analysis.error")) != 1 {
				t.Error("want analysis.error")
			}
			if r := warnReasons(t, e.c); len(r) != 0 {
				t.Errorf("warnings %q stand in for the failure", r)
			}
		})
	}
}

func TestRecoverNeverModifiesSource(t *testing.T) {
	e := newRecEnv(t, delNode("/a.bin", pat(1, 2*bs)), delNode("/z.bin", make([]byte, bs)))
	p := e.s.Parent
	before, err := os.Stat(filepath.Join(e.c.Dir, filepath.FromSlash(p.Path)))
	if err != nil {
		t.Fatal(err)
	}
	e.recover(t, examine.RecoverOptions{All: true})
	raw := readArtifact(t, e.c, p)
	if sha256hex(raw) != p.SHA256 || int64(len(raw)) != p.Size || before.Size() != p.Size {
		t.Error("the parent image changed")
	}
}

func TestRecoverOutputBudget(t *testing.T) {
	var nodes []fstest.Node
	for i := range 5 {
		nodes = append(nodes, delNode(fmt.Sprintf("/f%d.bin", i), pat(byte(i+1), 2*bs)))
	}
	for _, tc := range []struct {
		name         string
		o            examine.RecoverOptions
		files        int
		notProcessed int
		limit        string
	}{
		{"max files", examine.RecoverOptions{MaxFiles: 3}, 3, 2, "max-files"},
		{"exact files", examine.RecoverOptions{MaxFiles: 5}, 5, 0, ""},
		{"max bytes", examine.RecoverOptions{MaxBytes: 4*bs + bs}, 2, 3, "max-bytes"},
		{"exact bytes", examine.RecoverOptions{MaxBytes: 10 * bs}, 5, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRecEnv(t, nodes...)
			tc.o.All = true
			sum := e.recover(t, tc.o)
			if got := len(recovered(t, e.c)); got != tc.files || sum.Recovered != tc.files {
				t.Errorf("%d artifacts, want %d", got, tc.files)
			}
			if sum.LimitReached != tc.limit || sum.NotProcessed != tc.notProcessed {
				t.Errorf("limit %q not processed %d, want %q %d", sum.LimitReached, sum.NotProcessed, tc.limit, tc.notProcessed)
			}
			limitWarnings := 0
			for _, r := range warnReasons(t, e.c) {
				if strings.Contains(r, "limit") {
					limitWarnings++
				}
			}
			if (tc.limit != "") != (limitWarnings == 1) {
				t.Errorf("%d limit warnings for limit %q", limitWarnings, tc.limit)
			}
			verifyOK(t, e.c)
		})
	}
}

func TestRecoverFreeSpaceCheckBeforeFirstWrite(t *testing.T) {
	e := newRecEnv(t, delNode("/a.bin", pat(1, 2*bs)))
	e.s.SetFreeBytes(func(string) (int64, error) { return 1, nil })
	_, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if !errors.Is(err, examine.ErrInsufficientSpace) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}
	if len(auditByAction(t, e.c, "analysis.start")) != 1 || len(auditByAction(t, e.c, "analysis.error")) != 1 {
		t.Error("want analysis.start then analysis.error")
	}
	if n := len(recovered(t, e.c)); n != 0 {
		t.Errorf("%d artifacts written", n)
	}
}

func TestRecoverOutputPassesVerifyRules(t *testing.T) {
	nodes := []fstest.Node{
		delNode("/a-cut.bin", pat(1, 4*bs)),
		{Path: "/b-live.bin", Data: pat(2, 2*bs)},
		delNode("/c-uniform.bin", make([]byte, bs)), delNode("/d-ok.bin", pat(3, 2*bs)),
		delNode("/f-one.bin", pat(5, 2*bs)), delNode("/g-two.bin", pat(6, 2*bs)),
	}
	lay := layout(t, nodes)
	nodes[0].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: 6 * bs, Runs: []filesys.Run{lay["/a-cut.bin"][0], lay["/b-live.bin"][0]}}}
	nodes[5].Recover = []fstest.RecoverMap{{Method: "ext-inode-intact", Size: 2 * bs, Runs: lay["/f-one.bin"]}} // overlaps f-one
	e := newRecEnv(t, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true, KeepUniform: true})
	if sum.Partial != 1 || sum.Overlap != 2 || sum.Uniform != 1 {
		t.Fatalf("the mix lacks a case: %+v", sum)
	}
	// a second image in the same case, with a file that needs a runs sidecar
	buildBS = 512
	defer func() { buildBS = bs }()
	const n = evidence.MaxInlineRuns + 2
	many := fstest.Node{Path: "/e-many.bin", Data: pat(4, n*512), Deleted: true, Freed: true, Fragments: n, Recover: []fstest.RecoverMap{{Method: mtfsRecM}}}
	img2 := disk(fstest.Build(fstest.BuildSpec{Label: "L", BlockSize: 512, FreeBlocks: 4, Nodes: []fstest.Node{many}}))
	recs2 := importImage(t, e.c, img2, 1)
	s2 := openSession(t, e.c, recs2[0].ID)
	sum2, err := s2.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if err != nil || len(sum2.Artifacts) != 2 {
		t.Fatalf("second image: %v, %d artifacts", err, len(sum2.Artifacts))
	}
	verifyOK(t, e.c)
}

// FB-1: a copy interrupted in a file with more than MaxInlineRuns runs leaves a case that
// verifies: the full declared run list (written before the copy) stays referenced through
// Recovery.DeclaredRunsArtifact, the captured prefix is recorded as usual and reproduces.
func TestRecoverInterruptedManyRunsFileLeavesVerifiableCase(t *testing.T) {
	const n = evidence.MaxInlineRuns + 4
	const sbs = 512
	for _, tc := range []struct {
		name   string
		cutAt  int // fragments copied before the interruption
		cancel bool
		inline bool // the captured prefix fits inline
	}{
		{"cancel inline prefix", 2000, true, true},
		{"cancel prefix sidecar", 4098, true, false},
		{"read error inline prefix", 2000, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buildBS = sbs
			t.Cleanup(func() { buildBS = bs })
			data := pat(1, n*sbs)
			node := fstest.Node{Path: "/many.bin", Data: data, Deleted: true, Freed: true, Fragments: n, Recover: []fstest.RecoverMap{{Method: mtfsRecM}}}
			e := newRecEnv(t, node)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := examine.RecoverOptions{Partition: -1, All: true}
			if tc.cancel {
				opts.Progress = func(done, _ int64) {
					if done >= int64(tc.cutAt)*sbs {
						cancel()
					}
				}
			} else {
				fi := &failingImage{Image: e.s.Image, failOff: partStart + e.layout["/many.bin"][tc.cutAt].Offset}
				e.s.Image = fi
				opts.Progress = func(_, _ int64) { fi.armed = true }
			}
			_, err := e.s.Recover(ctx, opts)
			if tc.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want a cancel", err)
			}
			recs, _ := e.c.Manifest()
			var file evidence.ManifestRecord
			for _, r := range recs {
				if r.Source.Kind == evidence.KindRecover {
					file = r
				}
			}
			if file.ID == "" || !file.Incomplete {
				t.Fatalf("want one incomplete recovered artifact, manifest %d records", len(recs))
			}
			d := file.Source.Derived
			decl := d.Recovery.DeclaredRunsArtifact
			if decl == "" {
				t.Fatal("the interrupted artifact does not reference the declared full run list")
			}
			if (len(d.Runs) > 0) != tc.inline || (d.RunsArtifact != "") == tc.inline || d.RunsArtifact == decl {
				t.Errorf("captured prefix: %d inline runs, sidecar %q, declared %q (inline=%v)", len(d.Runs), d.RunsArtifact, decl, tc.inline)
			}
			full := file
			fd := *d
			fd.Runs, fd.RunsArtifact = nil, decl
			full.Source.Derived = &fd
			all, derr := e.c.DerivedRuns(full, nil)
			if derr != nil || len(all) != n {
				t.Errorf("declared list: %d runs, %v, want %d", len(all), derr, n)
			}
			rep, verr := e.c.VerifyWith(context.Background(), examine.RecoveredCheck())
			if verr != nil {
				t.Fatal(verr)
			}
			if !rep.OK() {
				t.Fatalf("case verify problems: %v", rep.Problems)
			}
		})
	}
}
