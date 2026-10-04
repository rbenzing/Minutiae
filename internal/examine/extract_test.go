package examine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

const blk = 512 // fstest default block size

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7+i/251) + seed
	}
	return b
}

// session builds a one-partition GPT disk around the MTFS nodes, imports it
// and opens it.
func session(t *testing.T, c *evidence.Case, free int, nodes ...fstest.Node) (*examine.Session, []byte) {
	t.Helper()
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: free, Nodes: nodes}))
	recs := importImage(t, c, data, 1)
	return openSession(t, c, recs[0].ID), data
}

// hookFS wraps an MTFS filesystem to inject faults.
type hookFS struct {
	mapEntry   func(filesys.Entry) filesys.Entry              // applied to ReadDir and Lookup results
	mapFile    func(filesys.Entry, filesys.File) filesys.File // applied to Open results
	mapUnalloc func([]filesys.Run) []filesys.Run              // applied to Unallocated results
}

type hookedFS struct {
	filesys.FileSystem
	h hookFS
}

func (f hookedFS) mapE(e filesys.Entry) filesys.Entry {
	if f.h.mapEntry != nil {
		return f.h.mapEntry(e)
	}
	return e
}

func (f hookedFS) ReadDir(d filesys.Entry) ([]filesys.Entry, error) {
	es, err := f.FileSystem.ReadDir(d)
	for i := range es {
		es[i] = f.mapE(es[i])
	}
	return es, err
}

func (f hookedFS) Lookup(p string) (filesys.Entry, error) {
	e, err := f.FileSystem.Lookup(p)
	return f.mapE(e), err
}

func (f hookedFS) Unallocated() ([]filesys.Run, error) {
	rs, err := f.FileSystem.Unallocated()
	if err == nil && f.h.mapUnalloc != nil {
		rs = f.h.mapUnalloc(rs)
	}
	return rs, err
}

func (f hookedFS) Open(e filesys.Entry) (filesys.File, error) {
	file, err := f.FileSystem.Open(e)
	if err == nil && f.h.mapFile != nil {
		file = f.h.mapFile(e, file)
	}
	return file, err
}

// runsFile reports fixed runs.
type runsFile struct {
	filesys.File
	runs []filesys.Run
}

func (f runsFile) Runs() []filesys.Run { return f.runs }

// failingFile fails reads that reach past offset after.
type failingFile struct {
	filesys.File
	after int64
}

func (f failingFile) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.after {
		n := 0
		if off < f.after {
			n, _ = f.File.ReadAt(p[:f.after-off], off)
		}
		return n, &filesys.CorruptError{Structure: "test", Offset: off, Reason: "injected read failure"}
	}
	return f.File.ReadAt(p, off)
}

// sessionHook is session with the filesystem wrapped by hook.
func sessionHook(t *testing.T, c *evidence.Case, hook hookFS, free int, nodes ...fstest.Node) (*examine.Session, []byte) {
	t.Helper()
	data := disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: free, Nodes: nodes}))
	recs := importImage(t, c, data, 1)
	opts := examine.Options{Drivers: []detect.Driver{{
		Name: "mtfs", Probe: fstest.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := fstest.Open(r, size)
			if err != nil {
				return nil, err
			}
			return hookedFS{FileSystem: fsys, h: hook}, nil
		},
	}}}
	s, err := examine.Open(c, recs[0].ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, data
}

func readArtifact(t *testing.T, c *evidence.Case, rec evidence.ManifestRecord) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(rec.Path)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func auditEntries(t *testing.T, c *evidence.Case) []evidence.AuditEntry {
	t.Helper()
	es, err := evidence.ReadAuditEntries(filepath.Join(c.Dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func auditByAction(t *testing.T, c *evidence.Case, action string) []evidence.AuditEntry {
	t.Helper()
	var out []evidence.AuditEntry
	for _, e := range auditEntries(t, c) {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func verifyOK(t *testing.T, c *evidence.Case) {
	t.Helper()
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("case verify problems: %v", rep.Problems)
	}
}

// readImageRuns concatenates the image bytes at the runs; a hole reads zeros.
func readImageRuns(img []byte, runs []evidence.Run) []byte {
	var out []byte
	for _, r := range runs {
		if r.Offset < 0 {
			out = append(out, make([]byte, r.Length)...)
			continue
		}
		out = append(out, img[r.Offset:r.Offset+r.Length]...)
	}
	return out
}

func extractAll(t *testing.T, s *examine.Session, o examine.ExtractOptions) examine.Summary {
	t.Helper()
	sum, err := s.Extract(context.Background(), o)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return sum
}

func TestExtractRecordsProvenance(t *testing.T) {
	c := newCase(t)
	content := pattern(5*blk+100, 1)
	s, img := session(t, c, 2,
		fstest.Node{Path: "/docs/a.txt", Data: content, Fragments: 2, MTime: 1700000000, Mode: 0o640, UID: 1000, GID: 100},
		fstest.Node{Path: "/other.txt", Data: []byte("zz")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/docs/a.txt"}})
	if sum.Files != 1 || sum.Bytes != int64(len(content)) || sum.Skipped != 0 || len(sum.Artifacts) != 1 || sum.AnalysisID == "" {
		t.Fatalf("summary = %+v", sum)
	}
	rec := sum.Artifacts[0]
	if !strings.HasSuffix(rec.Path, "/"+sum.AnalysisID+"/p1-mtfs/docs/a.txt") || !strings.Contains(rec.Path, "/img/") {
		t.Errorf("path = %q", rec.Path)
	}
	if rec.SHA256 != sha256hex(content) || rec.Incomplete || rec.Size != int64(len(content)) {
		t.Errorf("record = %+v", rec)
	}
	src := rec.Source
	if src.Kind != "extract" || src.DeviceID != "img" || src.RemotePath != "/docs/a.txt" || src.Derived == nil {
		t.Fatalf("source = %+v", src)
	}
	d := src.Derived
	part := s.Table.Partitions[0]
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	e, err := fsys.Lookup("/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	f, err := fsys.Open(e)
	if err != nil {
		t.Fatal(err)
	}
	fsRuns := f.Runs()
	if len(fsRuns) != 2 {
		t.Fatalf("setup: fs runs = %v, want 2 fragments", fsRuns)
	}
	var want []evidence.Run
	for _, r := range fsRuns {
		want = append(want, evidence.Run{Offset: r.Offset + part.Start, Length: r.Length})
	}
	if d.ParentID != s.Parent.ID || d.ParentSHA256 != s.Parent.SHA256 || d.ParentIncomplete ||
		d.Partition != 1 || d.PartitionOffset != part.Start ||
		d.FSType != "mtfs" || d.FSPath != "/docs/a.txt" || d.FSID != e.ID ||
		d.Mode != e.Mode || d.UID != 1000 || d.GID != 100 || d.Encrypted || d.RunsArtifact != "" {
		t.Errorf("derived = %+v (entry %+v)", d, e)
	}
	if d.Mode&0o777 != 0o640 {
		t.Errorf("mode = %o", d.Mode)
	}
	if got := d.Times["modified"]; got != "2023-11-14T22:13:20Z" || len(d.Times) != 1 {
		t.Errorf("times = %v", d.Times)
	}
	if len(d.Runs) != len(want) {
		t.Fatalf("runs = %v, want %v", d.Runs, want)
	}
	for i := range want {
		if d.Runs[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, d.Runs[i], want[i])
		}
	}
	if !bytes.Equal(readArtifact(t, c, rec), content) {
		t.Error("artifact bytes differ")
	}
	if !bytes.Equal(readImageRuns(img, d.Runs), content) {
		t.Error("recorded runs do not reproduce the content")
	}
	// The manifest record carries the same derivation.
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	last := recs[len(recs)-1]
	if last.ID != rec.ID || last.Source.Derived == nil || last.Source.Derived.FSID != e.ID {
		t.Errorf("manifest record = %+v", last)
	}
	// Audit order: analysis.start precedes every artifact.create of the
	// analysis, and analysis.end is the last entry.
	var startSeq, createSeq int64
	entries := auditEntries(t, c)
	for _, e := range entries {
		switch {
		case e.Action == "analysis.start" && e.Details["analysis_id"] == sum.AnalysisID:
			startSeq = e.Seq
		case e.Action == "artifact.create" && e.Details["id"] == rec.ID:
			createSeq = e.Seq
		}
	}
	if endEntry := entries[len(entries)-1]; startSeq == 0 || createSeq <= startSeq ||
		endEntry.Action != "analysis.end" || endEntry.Details["analysis_id"] != sum.AnalysisID || endEntry.Seq <= createSeq {
		t.Errorf("audit order: start seq %d, artifact.create seq %d, last entry %s seq %d", startSeq, createSeq, endEntry.Action, endEntry.Seq)
	}
	verifyOK(t, c)
}

func TestExtractTimeFormats(t *testing.T) {
	c := newCase(t)
	s, _ := sessionHook(t, c, hookFS{mapEntry: func(e filesys.Entry) filesys.Entry {
		if e.Name == "a" {
			e.Times = filesys.Times{
				Modified: filesys.Timestamp{T: time.Date(2024, 2, 29, 1, 2, 3, 123456789, time.UTC), ZoneKnown: true},
				Accessed: filesys.Timestamp{T: time.Date(2024, 2, 29, 1, 2, 3, 500000000, time.UTC)},
				Created:  filesys.Timestamp{T: time.Date(2024, 2, 29, 1, 2, 3, 0, time.UTC)},
			}
		}
		return e
	}}, 0, fstest.Node{Path: "/a", Data: []byte("x")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/a"}})
	d := sum.Artifacts[0].Source.Derived
	want := map[string]string{
		"modified": "2024-02-29T01:02:03.123456789Z",
		"accessed": "2024-02-29T01:02:03.5",
		"created":  "2024-02-29T01:02:03",
	}
	if len(d.Times) != len(want) {
		t.Fatalf("times = %v", d.Times)
	}
	for k, v := range want {
		if d.Times[k] != v {
			t.Errorf("times[%s] = %q, want %q", k, d.Times[k], v)
		}
	}
}

func TestExtractRunsReproduceContent(t *testing.T) {
	c := newCase(t)
	hole := pattern(4*blk, 9)
	s, img := session(t, c, 2,
		fstest.Node{Path: "/hole.bin", Data: hole, Hole: true},
		fstest.Node{Path: "/frag.bin", Data: pattern(7*blk+3, 4), Fragments: 3},
		fstest.Node{Path: "/empty", Data: nil})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 3 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	byPath := map[string]evidence.ManifestRecord{}
	for _, r := range sum.Artifacts {
		byPath[r.Source.RemotePath] = r
	}
	h := byPath["/hole.bin"]
	d := h.Source.Derived
	sawHole := false
	for _, r := range d.Runs {
		if r.Offset == -1 {
			sawHole = true
		}
	}
	if !sawHole {
		t.Fatalf("runs = %v, want a hole (-1)", d.Runs)
	}
	got := readArtifact(t, c, h)
	if !bytes.Equal(readImageRuns(img, d.Runs), got) {
		t.Error("hole file: runs do not reproduce the artifact")
	}
	if !bytes.Equal(got[blk:2*blk], make([]byte, blk)) {
		t.Error("hole file: hole block is not zero")
	}
	f := byPath["/frag.bin"]
	if !bytes.Equal(readImageRuns(img, f.Source.Derived.Runs), readArtifact(t, c, f)) || len(f.Source.Derived.Runs) != 3 {
		t.Errorf("fragmented file runs = %v", f.Source.Derived.Runs)
	}
	e := byPath["/empty"]
	if e.Size != 0 || len(e.Source.Derived.Runs) != 0 {
		t.Errorf("empty file = %+v", e)
	}
}

func TestExtractSymlinkIsLinkTargetWithoutRuns(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0,
		fstest.Node{Path: "/real", Data: []byte("data")},
		fstest.Node{Path: "/link", Link: "/real"})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/link"}})
	if sum.Files != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	rec := sum.Artifacts[0]
	if string(readArtifact(t, c, rec)) != "/real" {
		t.Errorf("symlink artifact = %q", readArtifact(t, c, rec))
	}
	if d := rec.Source.Derived; len(d.Runs) != 0 || d.RunsArtifact != "" || d.Mode&0o170000 != 0o120000 {
		t.Errorf("derived = %+v", d)
	}
	verifyOK(t, c)
}

func TestExtractRecursive(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 1,
		fstest.Node{Path: "/t/a.txt", Data: []byte("aaa")},
		fstest.Node{Path: "/t/sub/b.txt", Data: []byte("bbbb")},
		fstest.Node{Path: "/t/sub/deep/c.txt", Data: []byte("c")},
		fstest.Node{Path: "/t/sub/link", Link: "a.txt"},
		fstest.Node{Path: "/outside.txt", Data: []byte("no")})
	var lastDone int64
	sum := extractAll(t, s, examine.ExtractOptions{
		Partition: -1, Paths: []string{"/t"}, Recursive: true,
		Progress: func(done, _ int64) { lastDone = done },
	})
	if sum.Files != 4 || sum.Bytes != 3+4+1+5 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if lastDone != sum.Bytes {
		t.Errorf("progress done = %d, want %d", lastDone, sum.Bytes)
	}
	got := map[string]string{}
	for _, r := range sum.Artifacts {
		rel := strings.TrimPrefix(r.Path, path.Dir(r.Path[:strings.Index(r.Path, "/p1-mtfs/")])+"/")
		got[rel] = string(readArtifact(t, c, r))
	}
	wantFiles := map[string]string{
		sum.AnalysisID + "/p1-mtfs/t/a.txt":          "aaa",
		sum.AnalysisID + "/p1-mtfs/t/sub/b.txt":      "bbbb",
		sum.AnalysisID + "/p1-mtfs/t/sub/deep/c.txt": "c",
		sum.AnalysisID + "/p1-mtfs/t/sub/link":       "a.txt",
	}
	if len(got) != len(wantFiles) {
		t.Fatalf("artifacts = %v", got)
	}
	for k, v := range wantFiles {
		if got[k] != v {
			t.Errorf("artifact %q = %q, want %q", k, got[k], v)
		}
	}
	starts := auditByAction(t, c, "analysis.start")
	ends := auditByAction(t, c, "analysis.end")
	if len(starts) != 1 || len(ends) != 1 {
		t.Fatalf("start/end = %d/%d", len(starts), len(ends))
	}
	st := starts[0]
	if st.DeviceID != "img" || st.Details["analysis_id"] != sum.AnalysisID || st.Details["op"] != "extract" ||
		st.Details["parent_id"] != s.Parent.ID || detailNum(st.Details["partition"]) != 1 {
		t.Errorf("start = %+v", st)
	}
	if ps, _ := st.Details["paths"].([]any); len(ps) != 1 || ps[0] != "/t" {
		t.Errorf("start paths = %v", st.Details["paths"])
	}
	en := ends[0]
	if en.Details["analysis_id"] != sum.AnalysisID || detailNum(en.Details["files"]) != 4 || detailNum(en.Details["bytes"]) != 13 || detailNum(en.Details["skipped"]) != 0 {
		t.Errorf("end = %+v", en)
	}
	verifyOK(t, c)
}

func TestExtractSkipsDeletedWithWarning(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 1,
		fstest.Node{Path: "/d/live.txt", Data: []byte("live")},
		fstest.Node{Path: "/d/gone.txt", Data: []byte("gone"), Deleted: true},
		fstest.Node{Path: "/lonely.txt", Data: []byte("lonely"), Deleted: true})
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsys.ReadDir(fsys.Root())
	if err != nil {
		t.Fatal(err)
	}
	var lonelyID string
	for _, e := range root {
		if e.Name == "lonely.txt" {
			lonelyID = e.ID
		}
	}
	if lonelyID == "" {
		t.Fatal("setup: lonely.txt not listed")
	}
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"id:" + lonelyID, "/d"}, Recursive: true})
	if sum.Files != 1 || sum.Skipped != 2 || len(sum.Artifacts) != 1 || sum.Artifacts[0].Source.RemotePath != "/d/live.txt" {
		t.Fatalf("summary = %+v", sum)
	}
	ws := auditByAction(t, c, "analysis.warning")
	if len(ws) != 2 {
		t.Fatalf("warnings = %+v", ws)
	}
	if len(sum.Warnings) != 2 || sum.Warnings[0].Path != "/lonely.txt" || sum.Warnings[1].Path != "/d/gone.txt" ||
		!strings.Contains(sum.Warnings[0].Reason, "sub-project 3") || !strings.Contains(sum.Warnings[1].Reason, "sub-project 3") {
		t.Errorf("Summary.Warnings = %+v, want both skips with path and reason in order", sum.Warnings)
	}
	paths := map[string]bool{}
	for _, w := range ws {
		r, _ := w.Details["reason"].(string)
		if !strings.Contains(r, "sub-project 3") || w.Details["analysis_id"] != sum.AnalysisID {
			t.Errorf("warning = %+v", w)
		}
		p, _ := w.Details["path"].(string)
		paths[p] = true
	}
	if !paths["/lonely.txt"] || !paths["/d/gone.txt"] {
		t.Errorf("warning paths = %v", paths)
	}
	if en := auditByAction(t, c, "analysis.end"); len(en) != 1 || detailNum(en[0].Details["skipped"]) != 2 {
		t.Errorf("end = %+v", en)
	}
	verifyOK(t, c)
}

func TestExtractDirWithoutRecursiveIsError(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0, fstest.Node{Path: "/d/a", Data: []byte("a")})
	_, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"/d"}})
	if err == nil || !strings.Contains(err.Error(), "is a directory (use -r)") {
		t.Fatalf("err = %v", err)
	}
	if got := auditByAction(t, c, "analysis.start"); len(got) != 0 {
		t.Errorf("a usage error must not start an analysis: %+v", got)
	}
	if _, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"/missing"}}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("missing path err = %v", err)
	}
	if _, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1}); err == nil {
		t.Error("no paths accepted")
	}
}

func TestExtractNameCollisionRetries(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0,
		fstest.Node{Path: "/A.txt", Data: []byte("upper")},
		fstest.Node{Path: "/a.txt", Data: []byte("lower")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	byRemote := map[string]evidence.ManifestRecord{}
	for _, r := range sum.Artifacts {
		byRemote[r.Source.RemotePath] = r
	}
	a, b := byRemote["/A.txt"], byRemote["/a.txt"]
	if !strings.HasSuffix(a.Path, "/p1-mtfs/A.txt") || !strings.HasSuffix(b.Path, "/p1-mtfs/a~2.txt") {
		t.Errorf("paths = %q, %q", a.Path, b.Path)
	}
	if string(readArtifact(t, c, a)) != "upper" || string(readArtifact(t, c, b)) != "lower" {
		t.Error("contents differ")
	}
}

func TestExtractRetriesOnUnmodelledCollision(t *testing.T) {
	// The runs sidecar of /big is /big.runs.jsonl, a name LocalPaths does not
	// know; the real file /big.runs.jsonl then hits ErrArtifactExists and must
	// get the next candidate.
	c := newCase(t)
	const n = evidence.MaxInlineRuns + 1
	s, _ := session(t, c, 0,
		fstest.Node{Path: "/big", Data: pattern(n*blk, 2), Fragments: n},
		fstest.Node{Path: "/big.runs.jsonl", Data: []byte("real file")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 2 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	var realFile evidence.ManifestRecord
	for _, r := range sum.Artifacts {
		if r.Source.RemotePath == "/big.runs.jsonl" {
			realFile = r
		}
	}
	if !strings.HasSuffix(realFile.Path, "/p1-mtfs/big.runs~2.jsonl") || string(readArtifact(t, c, realFile)) != "real file" {
		t.Errorf("real file = %+v", realFile)
	}
	verifyOK(t, c)
}

func TestExtractGivesUpAfterManyCollisions(t *testing.T) {
	c := newCase(t)
	// While /x.txt is being opened, occupy every local name the retry loop
	// could try: the exclusive create must fail 16 times and the file be skipped.
	hook := hookFS{mapFile: func(_ filesys.Entry, f filesys.File) filesys.File {
		starts := auditByAction(t, c, "analysis.start")
		acq := starts[len(starts)-1].Details["analysis_id"].(string)
		dir := filepath.Join(c.Dir, "artifacts", "img", acq, "p1-mtfs")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		names := []string{"x.txt"}
		for n := 2; n <= 17; n++ {
			names = append(names, "x~"+strconv.Itoa(n)+".txt")
		}
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("squatter"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return f
	}}
	s, _ := sessionHook(t, c, hook, 0, fstest.Node{Path: "/x.txt", Data: []byte("x")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/x.txt"}})
	if sum.Files != 0 || sum.Skipped != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	ws := auditByAction(t, c, "analysis.warning")
	if len(ws) != 1 || !strings.Contains(ws[0].Details["reason"].(string), "no free local name") {
		t.Errorf("warnings = %+v", ws)
	}
}

func TestExtractCancelledKeepsPartial(t *testing.T) {
	c := newCase(t)
	content := pattern(300*blk, 5)
	s, _ := session(t, c, 0, fstest.Node{Path: "/big.bin", Data: content})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sum, err := s.Extract(ctx, examine.ExtractOptions{
		Partition: -1, Paths: []string{"/big.bin"},
		Progress: func(done, _ int64) {
			if done > 0 {
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(sum.Artifacts) != 1 || !sum.Artifacts[0].Incomplete || sum.Artifacts[0].Size <= 0 || sum.Artifacts[0].Size >= int64(len(content)) {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Files != 0 {
		t.Errorf("Files = %d, an incomplete artifact is not a finished file", sum.Files)
	}
	es := auditByAction(t, c, "analysis.error")
	if len(es) != 1 || !strings.Contains(es[0].Details["error"].(string), "canceled") || es[0].Details["analysis_id"] != sum.AnalysisID {
		t.Fatalf("analysis.error = %+v", es)
	}
	if len(auditByAction(t, c, "analysis.end")) != 0 {
		t.Error("analysis.end written after an error")
	}
	// The partial bytes are a prefix of the content and verify still passes.
	if got := readArtifact(t, c, sum.Artifacts[0]); !bytes.HasPrefix(content, got) {
		t.Error("partial artifact is not a prefix of the content")
	}
	verifyOK(t, c)
}

func TestExtractManyRunsUsesSidecar(t *testing.T) {
	c := newCase(t)
	const n = evidence.MaxInlineRuns + 1
	content := pattern(n*blk, 3)
	s, img := session(t, c, 0, fstest.Node{Path: "/frag.bin", Data: content, Fragments: n})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/frag.bin"}})
	if sum.Files != 1 || len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	side, file := sum.Artifacts[0], sum.Artifacts[1]
	if side.Source.Kind != "runs" || !strings.HasSuffix(side.Path, "/p1-mtfs/frag.bin.runs.jsonl") || !strings.HasSuffix(file.Path, "/p1-mtfs/frag.bin") {
		t.Fatalf("paths: sidecar %q file %q (kind %q)", side.Path, file.Path, side.Source.Kind)
	}
	d := file.Source.Derived
	if d.RunsArtifact != side.ID || d.Runs != nil {
		t.Errorf("derived runs: artifact=%q inline=%d", d.RunsArtifact, len(d.Runs))
	}
	var runs []evidence.Run
	for _, line := range strings.Split(strings.TrimSuffix(string(readArtifact(t, c, side)), "\n"), "\n") {
		var r evidence.Run
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
	}
	if len(runs) != n {
		t.Fatalf("sidecar lines = %d, want %d", len(runs), n)
	}
	if !bytes.Equal(readImageRuns(img, runs), content) {
		t.Error("sidecar runs do not reproduce the content")
	}
	verifyOK(t, c)
}

func TestExtractFromIncompleteParentIsFlagged(t *testing.T) {
	c := newCase(t)
	data := disk(fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{{Path: "/a.txt", Data: []byte("hello")}}}))
	rec, err := c.Capture("img", "acq", "image/x", evidence.Source{Kind: "import", DeviceID: "img", Segment: 1, Segments: 1},
		func(w io.Writer) error {
			if _, err := w.Write(data); err != nil {
				return err
			}
			return errors.New("interrupted")
		})
	if err == nil || !rec.Incomplete {
		t.Fatalf("setup: err=%v incomplete=%v", err, rec.Incomplete)
	}
	s := openSession(t, c, rec.ID)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/a.txt"}})
	if sum.Files != 1 || !sum.Artifacts[0].Source.Derived.ParentIncomplete {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestExtractWarnsOnBadRunsButStillExtracts(t *testing.T) {
	c := newCase(t)
	s, _ := sessionHook(t, c, hookFS{mapFile: func(_ filesys.Entry, f filesys.File) filesys.File {
		return runsFile{File: f, runs: []filesys.Run{{Offset: 0, Length: 7}}} // covers 7 of 5 bytes (a prefix would be valid; more than the file is not)
	}}, 0, fstest.Node{Path: "/a.txt", Data: []byte("hello")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/a.txt"}})
	if sum.Files != 1 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	d := sum.Artifacts[0].Source.Derived
	if len(d.Runs) != 0 || d.RunsArtifact != "" {
		t.Errorf("wrong provenance recorded: %+v", d)
	}
	if string(readArtifact(t, c, sum.Artifacts[0])) != "hello" {
		t.Error("content not extracted")
	}
	ws := auditByAction(t, c, "analysis.warning")
	if len(ws) != 1 || !strings.Contains(ws[0].Details["reason"].(string), "runs") {
		t.Errorf("warnings = %+v", ws)
	}
}

func TestExtractReadErrorWarnsAndContinues(t *testing.T) {
	c := newCase(t)
	s, _ := sessionHook(t, c, hookFS{mapFile: func(e filesys.Entry, f filesys.File) filesys.File {
		if e.Name == "a.txt" {
			return failingFile{File: f, after: blk}
		}
		return f
	}}, 0,
		fstest.Node{Path: "/a.txt", Data: pattern(4*blk, 1)},
		fstest.Node{Path: "/b.txt", Data: []byte("fine")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 1 || sum.Skipped != 1 || len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	if !sum.Artifacts[0].Incomplete || sum.Artifacts[1].Incomplete || string(readArtifact(t, c, sum.Artifacts[1])) != "fine" {
		t.Errorf("artifacts = %+v", sum.Artifacts)
	}
	if ws := auditByAction(t, c, "analysis.warning"); len(ws) != 1 {
		t.Errorf("warnings = %+v", ws)
	}
	verifyOK(t, c)
}

func TestExtractOtherTypeWarns(t *testing.T) {
	c := newCase(t)
	s, _ := sessionHook(t, c, hookFS{mapEntry: func(e filesys.Entry) filesys.Entry {
		if e.Name == "a.txt" {
			e.Type = filesys.TypeOther
		}
		return e
	}}, 0, fstest.Node{Path: "/a.txt", Data: []byte("x")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 0 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestExtractWholeImageUsesPartitionZero(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{{Path: "/a.txt", Data: []byte("hello")}}}), 1)
	s := openSession(t, c, recs[0].ID)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/a.txt"}})
	d := sum.Artifacts[0].Source.Derived
	if d.Partition != 0 || d.PartitionOffset != 0 || !strings.Contains(sum.Artifacts[0].Path, "/p0-mtfs/a.txt") {
		t.Errorf("derived = %+v path %q", d, sum.Artifacts[0].Path)
	}
}

func TestExtractNeverModifiesSource(t *testing.T) {
	c := newCase(t)
	data := disk(fstest.Build(fstest.BuildSpec{FreeBlocks: 3, Nodes: []fstest.Node{
		{Path: "/a.txt", Data: []byte("hello")}, {Path: "/d/b.txt", Data: pattern(3000, 1)},
	}}), make([]byte, 1024))
	recs := importImage(t, c, data, 2)
	before := map[string][2]any{}
	for _, r := range recs {
		p := filepath.Join(c.Dir, filepath.FromSlash(r.Path))
		sum, mt := fileState(t, p)
		before[p] = [2]any{sum, mt}
	}
	s := openSession(t, c, recs[0].ID)
	extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if _, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Volume: true}); err != nil {
		t.Fatal(err)
	}
	for p, b := range before {
		sum, mt := fileState(t, p)
		if sum != b[0] || mt != b[1] {
			t.Errorf("%s changed", p)
		}
	}
	for _, r := range recs {
		got, _, err := c.OpenArtifact(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		_ = got.Close()
	}
	verifyOK(t, c)
}

// detailNum reads an integer audit detail (decoded as json.Number).
func detailNum(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	}
	return -1
}

func TestExtractUnsafeEntryNameWarns(t *testing.T) {
	c := newCase(t)
	s, _ := sessionHook(t, c, hookFS{mapEntry: func(e filesys.Entry) filesys.Entry {
		if e.Name == "evil" {
			e.Name = "../x"
		}
		return e
	}}, 0, fstest.Node{Path: "/evil", Data: []byte("x")}, fstest.Node{Path: "/ok", Data: []byte("y")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 1 || sum.Skipped != 1 || sum.Artifacts[0].Source.RemotePath != "/ok" {
		t.Fatalf("summary = %+v", sum)
	}
	verifyOK(t, c)
}

func TestExtractFromSplitImageRecordsAllParentSegments(t *testing.T) {
	c := newCase(t)
	data := disk(fstest.Build(fstest.BuildSpec{FreeBlocks: 2, Nodes: []fstest.Node{{Path: "/a.txt", Data: []byte("hello")}}}))
	recs := importImage(t, c, data, 3)
	s := openSession(t, c, recs[1].ID)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/a.txt"}})
	ex, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []evidence.ManifestRecord{sum.Artifacts[0], ex.Artifacts[0], ex.Artifacts[1]} {
		d := rec.Source.Derived
		if d.ParentID != recs[0].ID || len(d.ParentSegments) != 3 {
			t.Fatalf("derived = %+v", d)
		}
		for i, ref := range d.ParentSegments {
			if ref.ID != recs[i].ID || ref.SHA256 != recs[i].SHA256 {
				t.Errorf("segment %d = %+v, want %s %s", i+1, ref, recs[i].ID, recs[i].SHA256)
			}
		}
	}
	verifyOK(t, c)
}

func TestExtractCaseWriteFailureIsNeverDowngradedToWarning(t *testing.T) {
	c := newCase(t)
	manifest := filepath.Join(c.Dir, "manifest.jsonl")
	t.Cleanup(func() { _ = os.Chmod(manifest, 0o600) })
	// While /a.txt is being read it hits a skippable (corrupt) error; at the
	// same time the manifest becomes unwritable, so keeping the partial
	// artifact fails. That must abort the run, not become a warning.
	s, _ := sessionHook(t, c, hookFS{mapFile: func(e filesys.Entry, f filesys.File) filesys.File {
		if e.Name == "a.txt" {
			if err := os.Chmod(manifest, 0o400); err != nil {
				t.Fatal(err)
			}
			return failingFile{File: f, after: blk}
		}
		return f
	}}, 0,
		fstest.Node{Path: "/a.txt", Data: pattern(4*blk, 1)},
		fstest.Node{Path: "/b.txt", Data: []byte("later")})
	if f, err := os.OpenFile(manifest, os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		_ = f.Close()
	}
	sum, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if probe, perr := os.OpenFile(manifest, os.O_APPEND|os.O_WRONLY, 0o600); perr == nil {
		_ = probe.Close()
		t.Skip("manifest stays writable despite chmod 0400 (running as a privileged user)")
	}
	if err == nil {
		t.Fatalf("Extract succeeded: %+v", sum)
	}
	var cw interface{ Unwrap() error }
	if !errors.As(err, &cw) {
		t.Errorf("err = %v", err)
	}
	if sum.Files != 0 || sum.Skipped != 0 {
		t.Errorf("summary = %+v, want no file counted and no warning", sum)
	}
	for _, r := range sum.Artifacts {
		if r.Source.RemotePath == "/b.txt" {
			t.Errorf("run continued after the case failure: %+v", sum.Artifacts)
		}
	}
	if ws := auditByAction(t, c, "analysis.warning"); len(ws) != 0 {
		t.Errorf("warnings = %+v", ws)
	}
	if es := auditByAction(t, c, "analysis.error"); len(es) != 1 {
		t.Errorf("analysis.error entries = %d", len(es))
	}
	if len(auditByAction(t, c, "analysis.end")) != 0 {
		t.Error("analysis.end written after a failure")
	}
}

// sparseFile pretends to be a file of the given size and runs; it reads as zeros.
type sparseFile struct {
	filesys.File
	size int64
	runs []filesys.Run
}

func (f sparseFile) Size() int64         { return f.size }
func (f sparseFile) Runs() []filesys.Run { return f.runs }
func (f sparseFile) ReadAt(p []byte, _ int64) (int, error) {
	clear(p)
	return len(p), nil
}

func TestExtractSkipsSparseFileLargerThanPartition(t *testing.T) {
	const huge = int64(1) << 50
	for name, f := range map[string]sparseFile{
		"hole run":                 {size: huge, runs: []filesys.Run{{Offset: -1, Length: huge}}},
		"hole runs overflow":       {size: 0, runs: []filesys.Run{{Offset: -1, Length: math.MaxInt64}, {Offset: -1, Length: math.MaxInt64}}},
		"no runs and a huge size":  {size: huge},
		"hole next to data blocks": {size: huge + 5, runs: []filesys.Run{{Offset: 0, Length: 5}, {Offset: -1, Length: huge}}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCase(t)
			s, _ := sessionHook(t, c, hookFS{mapFile: func(e filesys.Entry, file filesys.File) filesys.File {
				if e.Name != "bomb.bin" {
					return file
				}
				f.File = file
				return f
			}}, 0,
				fstest.Node{Path: "/bomb.bin", Data: []byte("hello")},
				fstest.Node{Path: "/ok.txt", Data: []byte("fine")})
			sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/bomb.bin", "/ok.txt"}})
			if sum.Files != 1 || sum.Skipped != 1 || len(sum.Artifacts) != 1 || sum.Bytes != 4 {
				t.Fatalf("summary = %+v", sum)
			}
			ws := auditByAction(t, c, "analysis.warning")
			if len(ws) != 1 || ws[0].Details["path"] != "/bomb.bin" ||
				ws[0].Details["reason"] != "sparse size exceeds partition length; not extracted" {
				t.Errorf("warnings = %+v", ws)
			}
			verifyOK(t, c)
		})
	}
}

func TestExtractSmallSparseAndInlineFilesAreExtracted(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0,
		fstest.Node{Path: "/holey.bin", Data: pattern(3*blk, 7), Hole: true},
		fstest.Node{Path: "/inline.txt", Data: []byte("tiny"), Inline: true})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/holey.bin", "/inline.txt"}})
	if sum.Files != 2 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v", sum)
	}
}

// Two different entries with the same path (the same name twice in one
// directory) are both extracted; only the same entry twice is a duplicate.
func TestExtractDedupesByPathAndEntryID(t *testing.T) {
	c := newCase(t)
	s, _ := sessionHook(t, c, hookFS{mapEntry: func(e filesys.Entry) filesys.Entry {
		if e.Name == "b.txt" {
			e.Name = "a.txt"
		}
		return e
	}}, 0,
		fstest.Node{Path: "/d/a.txt", Data: []byte("first")},
		fstest.Node{Path: "/d/b.txt", Data: []byte("second")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/d"}, Recursive: true})
	if sum.Files != 2 || sum.Skipped != 0 || sum.Bytes != int64(len("first")+len("second")) {
		t.Fatalf("summary = %+v", sum)
	}
	// The same directory requested twice still reports each file as a duplicate.
	again := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/d", "/d"}, Recursive: true})
	if again.Files != 2 || again.Skipped != 2 {
		t.Fatalf("overlapping summary = %+v", again)
	}
}

func TestExtractWarningsAreCappedButSkippedIsAuthoritative(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 1, fstest.Node{Path: "/gone.txt", Data: []byte("gone"), Deleted: true})
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsys.ReadDir(fsys.Root())
	if err != nil || len(root) != 1 {
		t.Fatalf("setup: %v %+v", err, root)
	}
	const n = 130
	paths := make([]string, n)
	for i := range paths {
		paths[i] = "id:" + root[0].ID
	}
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: paths})
	if sum.Skipped != n {
		t.Fatalf("Skipped = %d, want %d (the count stays exact)", sum.Skipped, n)
	}
	if len(sum.Warnings) != 100 {
		t.Errorf("len(Warnings) = %d, want it capped at 100", len(sum.Warnings))
	}
	if got := len(auditByAction(t, c, "analysis.warning")); got != n {
		t.Errorf("audit has %d analysis.warning entries, want all %d", got, n)
	}
}

// encFile is a file that reports itself encrypted (as the APFS reader does for
// a file whose extents, not its listing, carry a key).
type encFile struct{ filesys.File }

func (encFile) Encrypted() bool { return true }

// A file that reports itself encrypted when opened makes the artifact carry
// Derived.Encrypted even though its listing entry was not flagged.
func TestExtractFlagsFileThatReportsItselfEncrypted(t *testing.T) {
	c := newCase(t)
	hook := hookFS{mapFile: func(e filesys.Entry, f filesys.File) filesys.File {
		if e.Name == "key.bin" {
			return encFile{f}
		}
		return f
	}}
	s, _ := sessionHook(t, c, hook, 0,
		fstest.Node{Path: "/key.bin", Data: []byte("ciphertext")},
		fstest.Node{Path: "/plain.bin", Data: []byte("plain")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	got := map[string]bool{}
	for _, a := range sum.Artifacts {
		if d := a.Source.Derived; d != nil {
			got[path.Base(a.Source.RemotePath)] = d.Encrypted
		}
	}
	if len(got) != 2 || !got["key.bin"] || got["plain.bin"] {
		t.Errorf("Derived.Encrypted by file = %v, want key.bin true and plain.bin false", got)
	}
}
